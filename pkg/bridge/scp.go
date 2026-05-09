package bridge

import (
	"bufio"
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
	for i := 1; i < len(args); i++ {
		arg := args[i]
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

		if arg == "--" {
			if i+1 >= len(args) {
				return scpRequest{}, fmt.Errorf("scp target path is required")
			}
			next := args[i+1]
			if req.Path != "" {
				return scpRequest{}, fmt.Errorf("multiple scp paths are not supported")
			}
			req.Path = next
			i++
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
	rc, wc := io.Pipe()
	resultCh, err := provider.Exec(context.Background(), ExecConfig{
		Input:  rc,
		Output: io.Discard,
		Cmd: []string{
			"/bin/sh", "-c",
			`target="$1"; name="$2"; if [ -d "$target" ]; then target="$target/$name"; fi; cat > "$target"`,
			"scp-upload", req.Path, header.Name,
		},
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

	rc, wc := io.Pipe()
	resultCh, err := provider.Exec(context.Background(), ExecConfig{
		Output: wc,
		Cmd: []string{
			"/bin/sh", "-c",
			`target="$1"; size=$(wc -c < "$target") && printf '%s\n' "$size" && cat "$target"`,
			"scp-download", req.Path,
		},
	})
	if err != nil {
		_ = rc.Close()
		_ = wc.Close()
		return err
	}

	execErrCh := make(chan error, 1)
	go func() {
		result := <-resultCh
		_ = wc.Close()
		if result.ExitCode != 0 {
			execErrCh <- fmt.Errorf("scp download exec failed with exit code %d", result.ExitCode)
			return
		}
		execErrCh <- nil
	}()

	execReader := bufio.NewReader(rc)
	sizeLine, err := execReader.ReadString('\n')
	if err != nil {
		return err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid scp download size: %w", err)
	}
	if size < 0 {
		return fmt.Errorf("invalid negative scp file size: %d", size)
	}

	filename := filepath.Base(req.Path)
	if _, err := fmt.Fprintf(ch, "C0644 %d %s\n", size, filename); err != nil {
		return err
	}
	if err := readSCPOK(reader); err != nil {
		return err
	}
	if _, err := io.CopyN(ch, execReader, size); err != nil {
		return err
	}
	if err := writeSCPOK(ch); err != nil {
		return err
	}
	if err := readSCPOK(reader); err != nil {
		return err
	}
	if err := <-execErrCh; err != nil {
		return err
	}
	return nil
}
