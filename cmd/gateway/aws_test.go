package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the credential transfer and cleanup paths without AWS, Terraform,
// or Kubernetes. The external commands are replaced; jq processes real JSON.
func TestAWSStoreCheck(t *testing.T) {
	for _, name := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("AWS script checks require %s: %v", name, err)
		}
	}
	for _, scenario := range []string{"success", "namespace-exists", "database-secret-fails", "redis-secret-fails", "invalid-secret", "pod-fails", "cleanup-fails"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			mock := filepath.Join(dir, "mock")
			if err := os.WriteFile(mock, []byte(awsStoreCheckMock), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"terraform", "aws", "kubectl", "curl", "timeout", "redis-cli", "openssl"} {
				if err := os.Symlink(mock, filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "deploy/aws/check-stores.sh")
			cmd.Dir = "../.."
			cmd.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "MOCK_DIR=" + dir, "SCENARIO=" + scenario}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (scenario == "success") {
				t.Fatalf("script result: %v\n%s", err, out)
			}
			if strings.Contains(string(out), "password-marker") || strings.Contains(string(out), strings.Repeat("r", 64)) {
				t.Fatal("credentials appeared in script output")
			}
			deleted, _ := os.ReadFile(filepath.Join(dir, "deleted"))
			created, _ := os.ReadFile(filepath.Join(dir, "created"))
			if scenario == "namespace-exists" {
				if len(deleted) != 0 {
					t.Fatal("deleted a namespace this run did not create")
				}
				return
			}
			if len(created) == 0 || string(created) != string(deleted) {
				t.Fatalf("created namespace %q, deleted %q", created, deleted)
			}
			work, err := os.ReadFile(filepath.Join(dir, "work"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(strings.TrimSpace(string(work))); !os.IsNotExist(err) {
				t.Fatalf("temporary files remain: %v", err)
			}
			secret, err := os.ReadFile(filepath.Join(dir, "secret.json"))
			if scenario == "success" || scenario == "pod-fails" || scenario == "cleanup-fails" {
				if err != nil {
					t.Fatal(err)
				}
				var got struct{ StringData map[string]string }
				if err := json.Unmarshal(secret, &got); err != nil {
					t.Fatal(err)
				}
				if got.StringData["PGPASSWORD"] != "password-marker" || got.StringData["REDISCLI_AUTH"] != strings.Repeat("r", 64) {
					t.Fatal("credentials did not reach the Secret intact")
				}
			} else if len(secret) != 0 {
				t.Fatal("created a Secret after credential retrieval or validation failed")
			}
			if scenario == "success" {
				for _, name := range []string{"postgres.sh", "valkey.sh", "valkey-plaintext.sh"} {
					if out, err := exec.CommandContext(ctx, "sh", "-n", filepath.Join(dir, name)).CombinedOutput(); err != nil {
						t.Fatalf("embedded script syntax: %v\n%s", err, out)
					}
				}
				for _, redisCase := range []string{"reset", "no-response", "plaintext-reply", "missing-tool"} {
					t.Run(redisCase, func(t *testing.T) {
						cmd := exec.CommandContext(ctx, "sh", filepath.Join(dir, "valkey-plaintext.sh"))
						cmd.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "REDIS_HOST=redis.example", "REDISCLI_AUTH=" + strings.Repeat("r", 64), "REDIS_CASE=" + redisCase}
						out, err := cmd.CombinedOutput()
						wantSuccess := redisCase == "reset" || redisCase == "no-response"
						if (err == nil) != wantSuccess {
							t.Fatalf("Valkey check result: %v\n%s", err, out)
						}
					})
				}
				for _, redisCase := range []string{"success", "auth-fails", "lua-fails", "lua-status-reply", "lua-wrong-length", "lua-wrong-content", "hostname-fails", "noauth-admitted", "noauth-tls-fails"} {
					t.Run(redisCase, func(t *testing.T) {
						cmd := exec.CommandContext(ctx, "sh", filepath.Join(dir, "valkey.sh"))
						cmd.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "REDIS_HOST=redis.example", "REDISCLI_AUTH=" + strings.Repeat("r", 64), "REDIS_CASE=" + redisCase}
						out, err := cmd.CombinedOutput()
						if (err == nil) != (redisCase == "success") {
							t.Fatalf("Valkey TLS check result: %v\n%s", err, out)
						}
						if strings.Contains(string(out), strings.Repeat("r", 64)) {
							t.Fatal("token appeared in TLS diagnostics")
						}
					})
				}
				testAWSValkeyTLS(t, filepath.Join(dir, "valkey.sh"))
			}
		})
	}
}

const awsStoreCheckMock = `#!/usr/bin/env bash
set -eu
case $(basename "$0") in
timeout)
  shift
  exec "$@" ;;
openssl)
  [[ -z ${REDISCLI_AUTH:-} ]] || exit 2
  [[ $* == *'-verify_hostname redis.example'* && $* == *-verify_return_error* && $* == *'-CAfile /check/amazon.pem'* ]] || exit 2
  [[ $REDIS_CASE != hostname-fails ]] || exit 1
  input=$(cat)
  if [[ $input == AUTH* ]]; then
    if [[ $REDIS_CASE == auth-fails ]]; then echo '-WRONGPASS'; exit; fi
    printf '+OK\r\n+PONG\r\n'
    case $REDIS_CASE in
      lua-fails) echo '-ERR' ;;
      lua-status-reply) printf '+ok\r\n' ;;
      lua-wrong-length) printf '$3\r\nok\r\n' ;;
      lua-wrong-content) printf '$2\r\nno\r\n' ;;
      *) printf '$2\r\nok\r\n' ;;
    esac
    printf '$22\r\nvalkey_version:9.1.0\r\n\r\n+OK\r\n'
  else
    [[ $REDIS_CASE != noauth-tls-fails ]] || exit 1
    if [[ $REDIS_CASE == noauth-admitted ]]; then printf '+PONG\r\n+OK\r\n'
    else printf '%s\r\n' '-NOAUTH Authentication required.' '+OK'; fi
  fi ;;
redis-cli)
  # redis-cli must only test plaintext rejection, without credentials.
  [[ $* != *--tls* && -z ${REDISCLI_AUTH:-} ]] || exit 2
  case $REDIS_CASE in
    reset) echo 'Error: Server closed the connection'; exit 1 ;;
    no-response) exit 124 ;;
    plaintext-reply) echo 'NOAUTH Authentication required.' ;;
    missing-tool) exit 127 ;;
  esac ;;
terraform)
  case ${@: -1} in
    database_address) echo db.example ;;
    database_name) echo gateway ;;
    database_master_secret_arn) echo database-secret ;;
    redis_address) echo redis.example ;;
    redis_auth_token_secret_name) echo redis-secret ;;
    *) exit 1 ;;
  esac ;;
aws)
  if [[ $* == *database-secret* ]]; then
    [[ $SCENARIO != database-secret-fails ]] || exit 1
    if [[ $SCENARIO == invalid-secret ]]; then echo '{}'
    else echo '{"username":"admin","password":"password-marker"}'; fi
  else
    [[ $SCENARIO != redis-secret-fails ]] || exit 1
    printf '%064d\n' 0 | tr 0 r
  fi ;;
curl)
  while (($#)); do
    if [[ $1 == -o ]]; then printf 'public CA fixture\n' >"$2"; exit; fi
    shift
  done
  echo 'public CA fixture' ;;
kubectl)
  while [[ $1 == --* ]]; do
    case $1 in --*=*) shift ;; *) shift 2 ;; esac
  done
  case "$1 $2" in
    'create namespace')
      [[ $SCENARIO != namespace-exists ]] || exit 1
      echo "$3" >"$MOCK_DIR/created" ;;
    'delete namespace')
      echo "$3" >"$MOCK_DIR/deleted"
      [[ $SCENARIO != cleanup-fails ]] || exit 1 ;;
    'create configmap')
      for arg in "$@"; do
        case $arg in
          --from-file=*/postgres.sh|--from-file=*/valkey.sh|--from-file=*/valkey-plaintext.sh)
            file=${arg#--from-file=}
            dirname "$file" >"$MOCK_DIR/work"
            cp "$file" "$MOCK_DIR/"
            ;;
        esac
      done ;;
    'create -f')
      input=$(cat)
      [[ -z $input ]] || printf '%s\n' "$input" >"$MOCK_DIR/secret.json" ;;
    'apply -f') cat >/dev/null ;;
    'get pod')
      if [[ $SCENARIO == pod-fails ]]; then echo Failed; else echo Succeeded; fi ;;
    'logs store-check') echo 'synthetic store result' ;;
    *) exit 1 ;;
  esac ;;
esac
`

// Use real OpenSSL and TLS, so a stub cannot conceal sending AUTH before
// hostname or CA verification. Only the local address and CA path are replaced.
func testAWSValkeyTLS(t *testing.T, scriptPath string) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Fatal("Valkey TLS regression tests require OpenSSL 3")
	}
	cert, certPEM := awsCheckCertificate(t)
	_, otherPEM := awsCheckCertificate(t)
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"verified", "wrong-hostname", "untrusted-ca"} {
		t.Run("real-tls/"+scenario, func(t *testing.T) {
			dir := t.TempDir()
			listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			requests := make(chan string, 4)
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					go awsCheckRESP(conn, requests)
				}
			}()
			caFile := filepath.Join(dir, "ca.pem")
			ca := certPEM
			if scenario == "untrusted-ca" {
				ca = otherPEM
			}
			if err := os.WriteFile(caFile, ca, 0600); err != nil {
				t.Fatal(err)
			}
			localScript := strings.ReplaceAll(string(script), "$REDIS_HOST:6379", listener.Addr().String())
			localScript = strings.ReplaceAll(localScript, "/check/amazon.pem", caFile)
			path := filepath.Join(dir, "valkey.sh")
			if err := os.WriteFile(path, []byte(localScript), 0600); err != nil {
				t.Fatal(err)
			}
			// macOS need not have GNU timeout; the subprocess context and server
			// deadline bound the real TLS test instead.
			if err := os.WriteFile(filepath.Join(dir, "timeout"), []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			host := "localhost"
			if scenario == "wrong-hostname" {
				host = "wrong.example"
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", path)
			cmd.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "REDIS_HOST=" + host, "REDISCLI_AUTH=" + strings.Repeat("r", 64)}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (scenario == "verified") {
				t.Fatalf("real TLS result: %v\n%s", err, out)
			}
			if strings.Contains(string(out), strings.Repeat("r", 64)) {
				t.Fatal("token appeared in TLS diagnostics")
			}
			connections := 1
			if scenario == "verified" {
				connections = 2
			}
			for i := range connections {
				select {
				case received := <-requests:
					if scenario != "verified" && received != "" {
						t.Fatal("sent application data before certificate verification")
					}
					if scenario == "verified" {
						if (i == 0) != strings.Contains(received, "AUTH "+strings.Repeat("r", 64)+"\r\n") {
							t.Fatal("unexpected AUTH transfer on the verified connection")
						}
					}
				case <-ctx.Done():
					t.Fatal("TLS server did not finish")
				}
			}
		})
	}
}

func awsCheckCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func awsCheckRESP(conn net.Conn, requests chan<- string) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	var received strings.Builder
	defer func() { requests <- received.String() }()
	reader := bufio.NewReader(conn)
	authenticated := false
	for {
		line, err := reader.ReadString('\n')
		received.WriteString(line)
		if err != nil {
			return
		}
		command := strings.TrimSuffix(line, "\r\n")
		switch {
		case command == "AUTH "+strings.Repeat("r", 64):
			authenticated = true
			fmt.Fprint(conn, "+OK\r\n")
		case command == "QUIT":
			fmt.Fprint(conn, "+OK\r\n")
			return
		case !authenticated:
			fmt.Fprint(conn, "-NOAUTH Authentication required.\r\n")
		case command == "PING":
			fmt.Fprint(conn, "+PONG\r\n")
		case command == "EVAL \"return 'ok'\" 0":
			fmt.Fprint(conn, "$2\r\nok\r\n")
		case command == "INFO server":
			info := "# Server\r\nvalkey_version:9.1.0\r\n"
			fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(info), info)
		default:
			fmt.Fprint(conn, "-ERR unexpected command\r\n")
		}
	}
}
