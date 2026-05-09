package bridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestParseSCPCommandUpload(t *testing.T) {
	req, err := parseSCPCommand("scp -t /tmp/file.txt")
	if err != nil {
		t.Fatalf("parseSCPCommand returned error: %v", err)
	}
	if req.Mode != scpModeUpload || req.Path != "/tmp/file.txt" {
		t.Fatalf("unexpected parse result: mode=%q path=%q", req.Mode, req.Path)
	}
}

func TestParseSCPCommandDownload(t *testing.T) {
	req, err := parseSCPCommand("scp -f /tmp/file.txt")
	if err != nil {
		t.Fatalf("parseSCPCommand returned error: %v", err)
	}
	if req.Mode != scpModeDownload || req.Path != "/tmp/file.txt" {
		t.Fatalf("unexpected parse result: mode=%q path=%q", req.Mode, req.Path)
	}
}

func TestParseSCPCommandRejectsNonSCP(t *testing.T) {
	_, err := parseSCPCommand("cat /tmp/file.txt")
	if err == nil {
		t.Fatal("expected non-scp command to be rejected")
	}
}

func TestParseSCPCommandPreservesQuotedPath(t *testing.T) {
	req, err := parseSCPCommand("scp -t '/tmp/file with spaces.txt'")
	if err != nil {
		t.Fatalf("parseSCPCommand returned error: %v", err)
	}
	if req.Mode != scpModeUpload || req.Path != "/tmp/file with spaces.txt" {
		t.Fatalf("unexpected parse result: mode=%q path=%q", req.Mode, req.Path)
	}
}

func TestParseSCPCommandRejectsRecursiveModeForPhase1(t *testing.T) {
	_, err := parseSCPCommand("scp -r -t /tmp/dir")
	if err == nil {
		t.Fatal("expected recursive scp command to be rejected")
	}
}

func TestReadSCPRecordReadsCreateFileHeader(t *testing.T) {
	cmd, args, err := readSCPRecord(bytes.NewBufferString("C0644 12 file.txt\n"))
	if err != nil {
		t.Fatalf("readSCPRecord returned error: %v", err)
	}
	if cmd != 'C' || args != "0644 12 file.txt" {
		t.Fatalf("unexpected record: cmd=%q args=%q", cmd, args)
	}
}

func TestParseSCPFileHeader(t *testing.T) {
	header, err := parseSCPFileHeader("0644 12 file.txt")
	if err != nil {
		t.Fatalf("parseSCPFileHeader returned error: %v", err)
	}
	if header.Mode != "0644" || header.Size != 12 || header.Name != "file.txt" {
		t.Fatalf("unexpected header: %#v", header)
	}
}

func TestParseSCPFileHeaderAllowsSpacesInFilename(t *testing.T) {
	header, err := parseSCPFileHeader("0644 12 file with spaces.txt")
	if err != nil {
		t.Fatalf("parseSCPFileHeader returned error: %v", err)
	}
	if header.Name != "file with spaces.txt" {
		t.Fatalf("unexpected filename: %q", header.Name)
	}
}

func TestParseSCPFileHeaderRejectsMalformedHeader(t *testing.T) {
	if _, err := parseSCPFileHeader("0644 not-a-size file.txt"); err == nil {
		t.Fatal("expected malformed C record to be rejected")
	}
}

func TestWriteAndReadSCPOK(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSCPOK(&buf); err != nil {
		t.Fatalf("writeSCPOK returned error: %v", err)
	}
	if got := buf.Bytes(); !bytes.Equal(got, []byte{0}) {
		t.Fatalf("expected single NUL ack, got %#v", got)
	}
	if err := readSCPOK(&buf); err != nil {
		t.Fatalf("readSCPOK returned error: %v", err)
	}
}

func TestReadSCPOKRejectsProtocolError(t *testing.T) {
	if err := readSCPOK(bytes.NewBufferString("\x01failure\n")); err == nil {
		t.Fatal("expected SCP error response to be rejected")
	}
}

type scpTestChannel struct {
	input  *bytes.Reader
	output bytes.Buffer
	stderr bytes.Buffer
}

func newSCPTestChannel(input []byte) *scpTestChannel {
	return &scpTestChannel{input: bytes.NewReader(input)}
}

func (c *scpTestChannel) Read(p []byte) (int, error)  { return c.input.Read(p) }
func (c *scpTestChannel) Write(p []byte) (int, error) { return c.output.Write(p) }
func (c *scpTestChannel) Close() error                { return nil }
func (c *scpTestChannel) CloseWrite() error           { return nil }
func (c *scpTestChannel) SendRequest(name string, wantReply bool, payload []byte) (bool, error) {
	return true, nil
}
func (c *scpTestChannel) Stderr() io.ReadWriter { return &c.stderr }

type scpRecordingProvider struct {
	execCalls      []ExecConfig
	inputBytes     bytes.Buffer
	downloadOutput []byte
	exitCode       int
	execErr        error
}

func (p *scpRecordingProvider) Resize(ctx context.Context, size ResizeOptions) error { return nil }

func (p *scpRecordingProvider) Exec(ctx context.Context, cfg ExecConfig) (<-chan ExecResult, error) {
	p.execCalls = append(p.execCalls, cfg)
	if p.execErr != nil {
		return nil, p.execErr
	}
	done := make(chan ExecResult, 1)
	go func() {
		if cfg.Input != nil {
			_, _ = io.Copy(&p.inputBytes, cfg.Input)
		}
		if cfg.Output != nil && len(p.downloadOutput) > 0 {
			_, _ = cfg.Output.Write(p.downloadOutput)
		}
		done <- ExecResult{ExitCode: p.exitCode}
	}()
	return done, nil
}

var _ ssh.Channel = (*scpTestChannel)(nil)
var _ SessionProvider = (*scpRecordingProvider)(nil)

func TestSCPUploadSingleFile(t *testing.T) {
	ch := newSCPTestChannel([]byte("C0644 11 hello.txt\nhello world\x00"))
	provider := &scpRecordingProvider{}

	err := scpUpload(ch, scpRequest{Mode: scpModeUpload, Path: "/tmp/hello.txt"}, provider)
	if err != nil {
		t.Fatalf("scpUpload returned error: %v", err)
	}

	if got := ch.output.Bytes(); !bytes.Equal(got, []byte{0, 0, 0}) {
		t.Fatalf("expected three SCP OK acks, got %#v", got)
	}
	if provider.inputBytes.String() != "hello world" {
		t.Fatalf("expected uploaded bytes to be copied to provider input, got %q", provider.inputBytes.String())
	}
	if len(provider.execCalls) != 1 {
		t.Fatalf("expected one provider exec call, got %d", len(provider.execCalls))
	}
	cmd := provider.execCalls[0].Cmd
	if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" || cmd[2] != "cat > '/tmp/hello.txt'" {
		t.Fatalf("unexpected upload command: %#v", cmd)
	}
}

func TestSCPDownloadSingleFile(t *testing.T) {
	ch := newSCPTestChannel([]byte{0, 0, 0})
	provider := &scpRecordingProvider{downloadOutput: []byte("11\nhello world")}

	err := scpDownload(ch, scpRequest{Mode: scpModeDownload, Path: "/tmp/file.txt"}, provider)
	if err != nil {
		t.Fatalf("scpDownload returned error: %v", err)
	}

	expected := []byte("C0644 11 file.txt\nhello world\x00")
	if got := ch.output.Bytes(); !bytes.Equal(got, expected) {
		t.Fatalf("unexpected download output: got %#v want %#v", got, expected)
	}
	if len(provider.execCalls) != 1 {
		t.Fatalf("expected one provider exec call, got %d", len(provider.execCalls))
	}
	cmd := provider.execCalls[0].Cmd
	if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" {
		t.Fatalf("unexpected download command: %#v", cmd)
	}
}

func TestHandleExecInterceptsSCPUpload(t *testing.T) {
	ch := newSCPTestChannel([]byte("C0644 11 hello.txt\nhello world\x00"))
	provider := &scpRecordingProvider{}
	s := &session{bridge: &Bridge{provider: provider}, channel: ch}
	payload := ssh.Marshal(struct{ Command string }{Command: "scp -t /tmp/hello.txt"})

	if err := s.handleExec(payload); err != nil {
		t.Fatalf("handleExec returned error: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for len(provider.execCalls) != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(provider.execCalls) != 1 {
		t.Fatalf("expected SCP handler to call provider exec once, got %d", len(provider.execCalls))
	}
	if provider.inputBytes.String() != "hello world" {
		t.Fatalf("expected uploaded bytes, got %q", provider.inputBytes.String())
	}
}

func TestSCPUploadMalformedHeaderWritesProtocolError(t *testing.T) {
	ch := newSCPTestChannel([]byte("C0644 not-a-size hello.txt\n"))
	err := scpUpload(ch, scpRequest{Mode: scpModeUpload, Path: "/tmp/hello.txt"}, &scpRecordingProvider{})
	if err == nil {
		t.Fatal("expected malformed header error")
	}
	got := ch.output.Bytes()
	if len(got) < 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("expected initial OK then SCP error, got %#v", got)
	}
}

func TestSCPUploadProviderExecErrorWritesProtocolError(t *testing.T) {
	ch := newSCPTestChannel([]byte("C0644 11 hello.txt\nhello world\x00"))
	provider := &scpRecordingProvider{execErr: errors.New("exec failed")}
	err := scpUpload(ch, scpRequest{Mode: scpModeUpload, Path: "/tmp/hello.txt"}, provider)
	if err == nil {
		t.Fatal("expected provider exec error")
	}
	got := ch.output.Bytes()
	if len(got) < 3 || got[0] != 0 || got[1] != 0 || got[2] != 1 {
		t.Fatalf("expected initial/header OKs then SCP error, got %#v", got)
	}
}
