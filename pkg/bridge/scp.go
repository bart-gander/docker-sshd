package bridge

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

type scpMode string

const (
	scpModeUpload   scpMode = "upload"
	scpModeDownload scpMode = "download"
)

type scpRequest struct {
	Mode scpMode
	Path string
}

type scpFileHeader struct {
	Mode string
	Size int64
	Name string
}

func parseSCPCommand(cmd string) (scpRequest, error) {
	args, err := splitExecCommand(cmd)
	if err != nil {
		return scpRequest{}, err
	}
	if len(args) == 0 || args[0] != "scp" {
		return scpRequest{}, fmt.Errorf("not an scp command")
	}

	var req scpRequest
	for _, arg := range args[1:] {
		if len(arg) == 0 {
			continue
		}
		if arg[0] != '-' {
			if req.Path != "" {
				return scpRequest{}, fmt.Errorf("multiple scp paths are not supported")
			}
			req.Path = arg
			continue
		}

		for _, flag := range arg[1:] {
			switch flag {
			case 't':
				req.Mode = scpModeUpload
			case 'f':
				req.Mode = scpModeDownload
			case 'd', 'p', 'v':
				// Accepted but not significant for the bridge implementation.
			case 'r':
				return scpRequest{}, fmt.Errorf("recursive scp is not supported")
			default:
				return scpRequest{}, fmt.Errorf("unsupported scp flag: -%c", flag)
			}
		}
	}

	if req.Mode == "" {
		return scpRequest{}, fmt.Errorf("scp mode flag -t or -f is required")
	}
	if req.Path == "" {
		return scpRequest{}, fmt.Errorf("scp target path is required")
	}
	return req, nil
}

func readSCPRecord(r io.Reader) (byte, string, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}

	cmd, err := br.ReadByte()
	if err != nil {
		return 0, "", err
	}

	line, err := br.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return cmd, line, nil
}

func parseSCPFileHeader(args string) (scpFileHeader, error) {
	parts := strings.SplitN(args, " ", 3)
	if len(parts) != 3 {
		return scpFileHeader{}, fmt.Errorf("invalid scp file header: %q", args)
	}
	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return scpFileHeader{}, fmt.Errorf("invalid scp file header: %q", args)
	}
	size, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return scpFileHeader{}, fmt.Errorf("invalid scp file size %q: %w", parts[1], err)
	}
	if size < 0 {
		return scpFileHeader{}, fmt.Errorf("invalid negative scp file size: %d", size)
	}
	return scpFileHeader{Mode: parts[0], Size: size, Name: parts[2]}, nil
}

func writeSCPOK(w io.Writer) error {
	_, err := w.Write([]byte{0})
	return err
}

func writeSCPError(w io.Writer, message string) error {
	_, err := fmt.Fprintf(w, "\x01%s\n", message)
	return err
}

func readSCPOK(r io.Reader) error {
	var status [1]byte
	if _, err := io.ReadFull(r, status[:]); err != nil {
		return err
	}
	switch status[0] {
	case 0:
		return nil
	case 1, 2:
		msg, _ := bufio.NewReader(r).ReadString('\n')
		return fmt.Errorf("scp remote error: %s", strings.TrimSpace(msg))
	default:
		return fmt.Errorf("unexpected scp status byte: %d", status[0])
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (s *session) handleSCP(req scpRequest) error {
	s.execLock.Lock()
	if s.execCalled {
		s.execLock.Unlock()
		return fmt.Errorf("exec already called")
	}
	s.execCalled = true
	s.execLock.Unlock()

	go func() {
		defer s.channel.Close()

		exitCode := uint32(0)
		var err error
		switch req.Mode {
		case scpModeUpload:
			err = scpUpload(s.channel, req, s.bridge.provider)
		case scpModeDownload:
			err = scpDownload(s.channel, req, s.bridge.provider)
		default:
			err = fmt.Errorf("unsupported scp mode %q", req.Mode)
		}
		if err != nil {
			log.Warnf("scp %s %q failed: %v", req.Mode, req.Path, err)
			exitCode = 1
		}

		ok, sendErr := s.channel.SendRequest("exit-status", false, ssh.Marshal(&struct{ uint32 }{exitCode}))
		log.Debugf("send scp exit status %v ok=%v err=%v", exitCode, ok, sendErr)
	}()

	return nil
}

func scpUpload(ch io.ReadWriter, req scpRequest, provider SessionProvider) error {
	reader := bufio.NewReader(ch)

	if err := writeSCPOK(ch); err != nil {
		return err
	}

	var header scpFileHeader
	for {
		cmd, args, err := readSCPRecord(reader)
		if err != nil {
			return err
		}
		switch cmd {
		case 'T':
			if err := writeSCPOK(ch); err != nil {
				return err
			}
			continue
		case 'C':
			header, err = parseSCPFileHeader(args)
			if err != nil {
				_ = writeSCPError(ch, err.Error())
				return err
			}
			if err := writeSCPOK(ch); err != nil {
				return err
			}
			goto haveHeader
		default:
			return fmt.Errorf("unexpected scp upload record %q", cmd)
		}
	}

haveHeader:
	targetPath := req.Path
	if strings.HasSuffix(targetPath, "/") {
		targetPath = filepath.Join(targetPath, header.Name)
	}

	rc, wc := io.Pipe()
	resultCh, err := provider.Exec(context.Background(), ExecConfig{
		Input:  rc,
		Output: io.Discard,
		Cmd:    []string{"/bin/sh", "-c", "cat > " + shellQuote(targetPath)},
	})
	if err != nil {
		_ = rc.Close()
		_ = wc.Close()
		_ = writeSCPError(ch, err.Error())
		return err
	}

	copyErrCh := make(chan error, 1)
	go func() {
		_, err := io.CopyN(wc, reader, header.Size)
		closeErr := wc.Close()
		if err != nil {
			copyErrCh <- err
			return
		}
		copyErrCh <- closeErr
	}()

	if err := <-copyErrCh; err != nil {
		_ = rc.Close()
		return err
	}
	if err := readSCPOK(reader); err != nil {
		return err
	}

	result := <-resultCh
	if result.ExitCode != 0 {
		return fmt.Errorf("scp upload exec failed with exit code %d", result.ExitCode)
	}

	return writeSCPOK(ch)
}

func scpDownload(ch io.ReadWriter, req scpRequest, provider SessionProvider) error {
	reader := bufio.NewReader(ch)
	if err := readSCPOK(reader); err != nil {
		return err
	}

	var output bytes.Buffer
	escapedPath := shellQuote(req.Path)
	cmd := "size=$(wc -c < " + escapedPath + ") && printf '%s\\n' \"$size\" && cat " + escapedPath
	resultCh, err := provider.Exec(context.Background(), ExecConfig{
		Output: &output,
		Cmd:    []string{"/bin/sh", "-c", cmd},
	})
	if err != nil {
		return err
	}
	result := <-resultCh
	if result.ExitCode != 0 {
		return fmt.Errorf("scp download exec failed with exit code %d", result.ExitCode)
	}

	payload := output.Bytes()
	nl := bytes.IndexByte(payload, '\n')
	if nl < 0 {
		return fmt.Errorf("scp download did not receive size line")
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(payload[:nl])), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid scp download size: %w", err)
	}
	if size < 0 || int64(len(payload[nl+1:])) < size {
		return fmt.Errorf("scp download output shorter than reported size")
	}
	data := payload[nl+1 : nl+1+int(size)]

	filename := filepath.Base(req.Path)
	if _, err := fmt.Fprintf(ch, "C0644 %d %s\n", size, filename); err != nil {
		return err
	}
	if err := readSCPOK(reader); err != nil {
		return err
	}
	if _, err := ch.Write(data); err != nil {
		return err
	}
	if err := writeSCPOK(ch); err != nil {
		return err
	}
	return readSCPOK(reader)
}
