# SCP Support for docker-sshd / kube-sshd

## Status

**Implemented.** Legacy SCP (single-file upload/download) is fully implemented and unit-tested.

- `pkg/bridge/scp.go` — SCP command detection, protocol handlers for upload and download.
- `pkg/bridge/scp_test.go` — unit tests covering command parsing, protocol records, upload, download, and error handling.
- `pkg/bridge/bridge.go` — SCP exec requests are intercepted via `parseSCPCommand` and routed to the SCP handler.

## Supported operations

```
scp -O local-file pod:/target/file   # upload
scp -O pod:/source/file ./local-file  # download
```

The `-O` flag forces legacy SCP mode (the modern default is SFTP, which is not supported).

## Limitations

Phase 1 does not include:
- SFTP subsystem support
- Recursive directory transfer
- Wildcards / globbing
- Remote-to-remote SCP

## How legacy SCP works over SSH

Legacy SCP does not use a custom SSH channel type. The client opens a normal SSH `session` channel and sends an `exec` request such as:

```
scp -t /tmp/file     # upload: server receives file
scp -f /tmp/file     # download: server sends file
```

After the `exec` request succeeds, both sides exchange a small byte protocol over the channel.

Upload flow, simplified:

1. Client sends exec request: `scp -t /target/path`.
2. Server replies success to the exec request.
3. Server writes one NUL byte (`\0`) to say it is ready.
4. Client sends optional time metadata: `T...\n`.
5. Client sends file header: `C0644 <size> <filename>\n`.
6. Server writes NUL ack.
7. Client sends exactly `<size>` raw file bytes.
8. Client sends one trailing NUL byte.
9. Server writes final NUL ack.

Download flow, simplified:

1. Client sends exec request: `scp -f /source/path`.
2. Server replies success to the exec request.
3. Client writes one NUL byte to request the file.
4. Server sends file header: `C0644 <size> <filename>\n`.
5. Client writes NUL ack.
6. Server sends exactly `<size>` raw file bytes.
7. Server sends one trailing NUL byte.
8. Client writes final NUL ack.

Directory recursion uses `D...` and `E` records. That is out of scope for Phase 1.

## Architecture

SCP `exec` requests are intercepted in `pkg/bridge/bridge.go` before forwarding the command to the target container/pod.

```
OpenSSH scp client
  -> SSH session exec: "scp -t /tmp/file" or "scp -f /tmp/file"
  -> bridge detects SCP via parseSCPCommand
  -> Go SCP handler speaks legacy SCP protocol on the SSH channel
  -> handler uses SessionProvider.Exec to run small commands:
       upload:   cat > target
       download: size + cat
```

Target container requires only `/bin/sh` and coreutils (`cat`, `wc`, `stat`).

## Verification

```bash
go test ./...
```

Manual local verification (once kube-sshd is running on port 2232):

```bash
printf 'hello scp\n' > /tmp/test-upload.txt
scp -O -P 2232 /tmp/test-upload.txt 'user@127.0.0.1:/tmp/test-upload.txt'
ssh -p 2232 user@127.0.0.1 'cat /tmp/test-upload.txt'

scp -O -P 2232 'user@127.0.0.1:/tmp/test-upload.txt' /tmp/test-download.txt
cmp /tmp/test-upload.txt /tmp/test-download.txt
```

Binary file test:

```bash
python3 - <<'PY'
from pathlib import Path
Path('/tmp/test.bin').write_bytes(bytes(range(256)) * 16)
PY
scp -O -P 2232 /tmp/test.bin 'user@127.0.0.1:/tmp/test.bin'
scp -O -P 2232 'user@127.0.0.1:/tmp/test.bin' /tmp/test.bin.out
cmp /tmp/test.bin /tmp/test.bin.out
```