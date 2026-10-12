package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The URL templates in deploy/aws/secrets.yaml are rendered the way External
// Secrets Operator 2.12 renders them (text/template, string data, missing
// keys rejected), with the placeholders deploy.sh replaces, and parsed by
// pgx and by the gateway's own Redis URL loader. A password with URL
// metacharacters must arrive unchanged.
func TestAWSSecretTemplates(t *testing.T) {
	manifest, err := os.ReadFile("../../deploy/aws/secrets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.NewReplacer(
		"__DATABASE_HOST__", "db.example.eu-north-1.rds.amazonaws.com",
		"__DATABASE_NAME__", "gateway",
		"__DATABASE_SECRET_ARN__", "arn:aws:secretsmanager:eu-north-1:123456789012:secret:rds!db-mock",
		"__REDIS_HOST__", "master.redis.example.cache.amazonaws.com",
	).Replace(string(manifest))
	if regexp.MustCompile(`__[A-Z_]+__`).MatchString(text) {
		t.Fatal("secrets.yaml has a placeholder deploy.sh does not replace")
	}

	const password = `p:a/s?s#w&o=r+d%x!*'(),;$~[]`
	token := strings.Repeat("T0k", 21) + "n"
	data := map[string]string{"username": "gateway_admin", "password": password, "token": token}
	render := func(src string) string {
		tmpl, err := template.New("url").Option("missingkey=error").Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		if err := tmpl.Execute(&out, data); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	urls := regexp.MustCompile(`(?m)^ +url: "(.*)"$`).FindAllStringSubmatch(text, -1)
	if len(urls) != 3 {
		t.Fatalf("found %d URL templates, want the owner, gateway, and Redis URLs", len(urls))
	}
	caPath := "/etc/llm-gateway/database-ca/ca.pem"
	values, err := os.ReadFile("../../deploy/helm/llm-gateway/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(values), caPath) {
		t.Fatalf("the chart no longer mounts the CA bundle at %s", caPath)
	}
	// pgx reads sslrootcert while parsing, so the parsed copy names a real
	// CA file in place of the chart's mount path.
	_, caPEM := awsCheckCertificate(t)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	users := map[string]bool{}
	for _, match := range urls {
		rendered := render(match[1])
		u, err := url.Parse(rendered)
		if err != nil {
			t.Fatalf("rendered URL does not parse: %v", err)
		}
		switch u.Scheme {
		case "postgres":
			if q := u.Query(); q.Get("sslmode") != "verify-full" || q.Get("sslrootcert") != caPath {
				t.Errorf("database URL for %s: sslmode %q, sslrootcert %q", u.User.Username(), q.Get("sslmode"), q.Get("sslrootcert"))
			}
			cfg, err := pgconn.ParseConfig(strings.Replace(rendered, "sslrootcert="+caPath, "sslrootcert="+caFile, 1))
			if err != nil {
				t.Fatalf("pgx rejects the database URL for %s: %v", u.User.Username(), err)
			}
			if cfg.Host != "db.example.eu-north-1.rds.amazonaws.com" || cfg.Port != 5432 || cfg.Database != "gateway" ||
				cfg.Password != password || cfg.TLSConfig == nil || cfg.TLSConfig.ServerName != cfg.Host || cfg.TLSConfig.RootCAs == nil {
				t.Errorf("pgx reads the URL for %s as host %q, port %d, database %q, password intact %v, verified TLS %v",
					cfg.User, cfg.Host, cfg.Port, cfg.Database, cfg.Password == password, cfg.TLSConfig != nil && cfg.TLSConfig.RootCAs != nil)
			}
			users[cfg.User] = true
		case "rediss":
			opts, err := loadRedis(func(name string) string {
				if name == redisURLVar {
					return rendered
				}
				return ""
			})
			if err != nil {
				t.Fatalf("the gateway rejects the Redis URL: %v", err)
			}
			if opts.Addr != "master.redis.example.cache.amazonaws.com:6379" || opts.Username != "" || opts.Password != token ||
				opts.DB != 0 || opts.TLSConfig == nil || opts.TLSConfig.ServerName != "master.redis.example.cache.amazonaws.com" {
				t.Errorf("the gateway reads the Redis URL as address %q, user %q, database %d, token intact %v, TLS %v",
					opts.Addr, opts.Username, opts.DB, opts.Password == token, opts.TLSConfig != nil)
			}
		default:
			t.Errorf("unexpected URL scheme %q", u.Scheme)
		}
	}
	if !users["gateway_admin"] || !users["gateway_app"] {
		t.Errorf("database URLs for users %v, want the master user and gateway_app", users)
	}

	// Every Secret the values file names is created by an ExternalSecret.
	awsValues, err := os.ReadFile("../../deploy/aws/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range regexp.MustCompile(`(?m)^ +(?:name|secretName): (llm-gateway-[a-z-]+)$`).FindAllStringSubmatch(string(awsValues), -1) {
		if name[1] == "llm-gateway-database-ca" {
			continue // a ConfigMap deploy.sh creates
		}
		if !strings.Contains(text, "  target:\n    name: "+name[1]+"\n") {
			t.Errorf("values name Secret %s, which no ExternalSecret creates", name[1])
		}
	}
}

// The operator must run in the namespace and under the service account the
// Pod Identity association grants the role to.
func TestAWSExternalSecretsIdentity(t *testing.T) {
	terraform, err := os.ReadFile("../../deploy/aws/platform/external-secrets.tf")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../../deploy/aws/deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	for tf, sh := range map[string]string{
		`external_secrets_namespace\s*=\s*"([^"]+)"`:       `(?m)^eso_namespace=(\S+)$`,
		`external_secrets_service_account\s*=\s*"([^"]+)"`: `(?m)^eso_service_account=(\S+)$`,
	} {
		want := regexp.MustCompile(tf).FindSubmatch(terraform)
		got := regexp.MustCompile(sh).FindSubmatch(script)
		if want == nil || got == nil || string(want[1]) != string(got[1]) {
			t.Errorf("deploy.sh installs the operator with %q, Terraform associates the role with %q", got, want)
		}
	}
}

// The digests the deploy mock reports for the two images.
const awsTestSHA = "0123456789abcdef0123456789abcdef01234567"

var (
	awsTestGatewayDigest = "sha256:aa" + strings.Repeat("0", 62)
	awsTestFakeDigest    = "sha256:bb" + strings.Repeat("0", 62)
)

// awsScript runs a deploy/aws script with fake external commands, returning
// its output, its error, and the mock's directory. Every invocation's
// arguments are logged to calls, so tests can check that no credential is
// ever passed as an argument.
func awsScript(t *testing.T, script, mockScript string, env ...string) (string, error, string) {
	t.Helper()
	for _, name := range []string{"bash", "jq", "openssl"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("AWS script checks require %s: %v", name, err)
		}
	}
	dir := t.TempDir()
	mock := filepath.Join(dir, "mock")
	if err := os.WriteFile(mock, []byte(mockScript), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"terraform", "aws", "kubectl", "helm", "docker", "curl", "git"} {
		if err := os.Symlink(mock, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Dir = "../.."
	cmd.Env = append([]string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"MOCK_DIR=" + dir,
		"AWS_SESSION_DIR=" + filepath.Join(dir, "session"),
	}, env...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err, dir
}

func readMockFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestAWSDeploy(t *testing.T) {
	failures := map[string]string{
		"image-missing": "Cannot resolve ghcr.io/ipekutku/llm-gateway:" + awsTestSHA,
		"sync-fails":    "External Secrets did not sync",
		"role-fails":    "The database role Job did not complete (conditions: Failed )",
	}
	for _, scenario := range []string{"empty-secrets", "filled-secrets", "image-missing", "sync-fails", "role-fails"} {
		t.Run(scenario, func(t *testing.T) {
			out, err, dir := awsScript(t, "deploy/aws/deploy.sh", awsDeployMock, "SCENARIO="+scenario, "AWS_IMAGE_SHA="+awsTestSHA)
			ok := scenario == "empty-secrets" || scenario == "filled-secrets"
			if (err == nil) != ok || !strings.Contains(out, failures[scenario]) {
				t.Fatalf("script result: %v\n%s", err, out)
			}
			calls := readMockFile(t, dir, "calls")
			helmGateway := strings.Contains(calls, "helm --kube-context llm-gateway-aws upgrade --install llm-gateway deploy/helm/llm-gateway")
			if helmGateway != ok {
				t.Fatalf("installed the gateway chart: %v\n%s", helmGateway, calls)
			}
			if scenario == "image-missing" && (strings.Contains(calls, "kubectl") || strings.Contains(calls, "put-secret-value")) {
				t.Fatal("changed the cluster or secrets without a resolvable image")
			}
			if scenario == "sync-fails" && strings.Contains(calls, "database-role") {
				t.Fatal("ran the database role Job before the Secrets were synced")
			}

			puts := strings.Count(calls, "put-secret-value")
			switch scenario {
			case "empty-secrets":
				if puts != 4 {
					t.Fatalf("filled %d secrets, want 4", puts)
				}
			case "filled-secrets":
				if puts != 0 {
					t.Fatal("replaced a secret that already had a value")
				}
				if !strings.Contains(out, "make aws-check needs them") {
					t.Fatal("no note about the missing client keys")
				}
			}

			if !ok {
				return
			}
			if !strings.Contains(calls, " --set image.tag="+awsTestSHA+"@"+awsTestGatewayDigest+" ") ||
				!strings.Contains(calls, " --set sidecars[0].image=ghcr.io/ipekutku/llm-gateway-fakeprovider:"+awsTestSHA+"@"+awsTestFakeDigest+" ") {
				t.Fatalf("images not pinned to their digests:\n%s", calls)
			}
			if !strings.Contains(calls, "oci://ghcr.io/external-secrets/charts/external-secrets@sha256:") ||
				!strings.Contains(calls, "--version 2.12.0") {
				t.Fatal("the operator chart is not pinned by version and digest")
			}
			if i, j := strings.Index(calls, "apply -f deploy/aws/database-role.yaml"), strings.Index(calls, "upgrade --install llm-gateway "); i < 0 || i > j {
				t.Fatal("the database role must be created before the chart")
			}
			secrets := readMockFile(t, dir, "secrets.yaml")
			if !strings.Contains(secrets, "key: arn:aws:secretsmanager:eu-north-1:123456789012:secret:rds!db-mock") ||
				!strings.Contains(secrets, "@db.example:5432/gateway?") || strings.Contains(secrets, "__") {
				t.Fatalf("External Secrets rendered incorrectly:\n%s", secrets)
			}

			if scenario != "empty-secrets" {
				return
			}
			var clients struct {
				Clients []struct {
					ID                string `json:"id"`
					KeySHA256         string `json:"key_sha256"`
					RequestsPerMinute int    `json:"requests_per_minute"`
					Burst             int    `json:"burst"`
				} `json:"clients"`
			}
			if err := json.Unmarshal([]byte(readMockFile(t, dir, "put-llm-gateway/clients")), &clients); err != nil {
				t.Fatal(err)
			}
			if len(clients.Clients) != 2 {
				t.Fatalf("clients file has %d clients, want 2", len(clients.Clients))
			}
			secretValues := []string{readMockFile(t, dir, "put-llm-gateway/database-app-password")}
			if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(secretValues[0]) {
				t.Fatal("app password is not 64 hexadecimal characters")
			}
			for _, c := range clients.Clients {
				path := filepath.Join(dir, "session", c.ID+"-key")
				key, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
					t.Errorf("%s has mode %v, want 0600", path, info.Mode().Perm())
				}
				sum := sha256.Sum256(key)
				if c.KeySHA256 != hex.EncodeToString(sum[:]) || len(key) < 48 {
					t.Errorf("client %s: stored hash does not match its session key", c.ID)
				}
				secretValues = append(secretValues, string(key))
			}
			if c := clients.Clients[1]; c.ID != "limited" || c.Burst != 10 || c.RequestsPerMinute != 6 {
				t.Errorf("limited client %+v, want bursts of 10 at 6 per minute, as aws-check expects", c)
			}
			for _, value := range secretValues {
				if strings.Contains(out, value) || strings.Contains(calls, value) {
					t.Fatal("a generated credential appeared in output or command arguments")
				}
			}
		})
	}
}

const awsDeployMock = `#!/usr/bin/env bash
set -eu
echo "$(basename "$0") $*" >>"$MOCK_DIR/calls"
case $(basename "$0") in
docker)
  [[ $SCENARIO != image-missing ]] || exit 1
  case $* in
    *ghcr.io/ipekutku/llm-gateway:*) printf '{"digest":"sha256:aa%062d"}\n' 0 ;;
    *ghcr.io/ipekutku/llm-gateway-fakeprovider:*) printf '{"digest":"sha256:bb%062d"}\n' 0 ;;
  esac ;;
terraform)
  case ${@: -1} in
    database_address) echo db.example ;;
    database_name) echo gateway ;;
    database_master_secret_arn) echo 'arn:aws:secretsmanager:eu-north-1:123456789012:secret:rds!db-mock' ;;
    redis_address) echo redis.example ;;
    *) exit 1 ;;
  esac ;;
aws)
  case $2 in
    describe-secret) if [[ $SCENARIO == filled-secrets ]]; then echo 1; else echo 0; fi ;;
    put-secret-value)
      while (($#)); do
        case $1 in
          --secret-id) id=$2 ;;
          --secret-string) file=${2#file://} ;;
        esac
        shift
      done
      mkdir -p "$MOCK_DIR/put-llm-gateway"
      cp "$file" "$MOCK_DIR/put-$id" ;;
    *) exit 1 ;;
  esac ;;
helm) ;;
curl)
  while (($#)); do
    if [[ $1 == -o ]]; then echo 'RDS CA fixture' >"$2"; fi
    shift
  done ;;
kubectl)
  while [[ $1 == --* ]]; do shift 2; done
  case "$1 $2" in
    'create namespace'|'create configmap') echo 'kind: Fixture' ;;
    'apply -f')
      if [[ $3 == - ]]; then cat >/dev/null
      elif [[ $3 == */secrets.yaml ]]; then cp "$3" "$MOCK_DIR/secrets.yaml"; fi ;;
    'wait --for=condition=Ready') [[ $SCENARIO != sync-fails ]] ;;
    'get externalsecret') echo 'synthetic status' ;;
    'delete job') ;;
    'get job') if [[ $SCENARIO == role-fails ]]; then echo 'Failed '; else echo 'Complete '; fi ;;
    'logs job/llm-gateway-database-role') echo 'gateway_app table privileges: INSERT, SELECT' ;;
    'get pods') ;;
    *) exit 1 ;;
  esac ;;
*) exit 1 ;;
esac
`

func TestAWSCheck(t *testing.T) {
	failures := map[string]string{
		"missing-keys":  "demo-key is missing",
		"not-shared":    "20 of 30 admitted: limits are not shared",
		"refilling":     "4 of 30 admitted, fewer than the burst",
		"local-limits":  "rate limits are not shared through Valkey",
		"usage-missing": "usage records check (pod phase: Failed)",
	}
	for _, scenario := range []string{"success", "missing-keys", "not-shared", "refilling", "local-limits", "usage-missing"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			keys := map[string]string{"demo": "aws-demo-" + strings.Repeat("d", 48), "limited": "aws-limited-" + strings.Repeat("l", 48)}
			env := []string{"SCENARIO=" + scenario}
			if scenario != "missing-keys" {
				session := filepath.Join(dir, "session")
				if err := os.Mkdir(session, 0700); err != nil {
					t.Fatal(err)
				}
				for name, key := range keys {
					if err := os.WriteFile(filepath.Join(session, name+"-key"), []byte(key), 0600); err != nil {
						t.Fatal(err)
					}
				}
				env = append(env, "AWS_SESSION_DIR="+session)
			}
			out, err, mockDir := awsScript(t, "deploy/aws/check.sh", awsCheckMock, env...)
			if (err == nil) != (scenario == "success") || !strings.Contains(out, failures[scenario]) {
				t.Fatalf("script result: %v\n%s", err, out)
			}
			calls := readMockFile(t, mockDir, "calls")
			for _, key := range keys {
				if strings.Contains(out, key) || strings.Contains(calls, key) {
					t.Fatal("a client key appeared in output or command arguments")
				}
			}
			if scenario != "success" {
				return
			}
			pod := readMockFile(t, mockDir, "usage-pod.yaml")
			if !strings.Contains(pod, "IN ('"+strings.Repeat("A", 26)+"', '"+strings.Repeat("B", 26)+"')") {
				t.Fatalf("usage query does not name both requests:\n%s", pod)
			}
			if !strings.Contains(out, "10 of 30 admitted") {
				t.Fatalf("missing shared-limit summary:\n%s", out)
			}
			if !strings.Contains(calls, "delete pod llm-gateway-check-usage") {
				t.Fatal("the usage pod was not deleted")
			}
		})
	}
}

// curl answers completions with request IDs AAAA… and BBBB…, and the
// limited client's requests with 404 (admitted) or 429 according to the
// scenario, counting them in a file.
const awsCheckMock = `#!/usr/bin/env bash
set -eu
echo "$(basename "$0") $*" >>"$MOCK_DIR/calls"
case $(basename "$0") in
kubectl)
  while [[ $1 == --* ]]; do shift 2; done
  case "$1 $2" in
    'rollout status') ;;
    port-forward*) exec sleep 30 ;;
    'get pods') printf 'gateway-a\ngateway-b\n' ;;
    'delete pod') ;;
    'apply -f') cat >"$MOCK_DIR/usage-pod.yaml" ;;
    'get pod') if [[ $SCENARIO == usage-missing ]]; then echo Failed; else echo Succeeded; fi ;;
    'logs llm-gateway-check-usage') echo 'synthetic usage rows' ;;
    *) exit 1 ;;
  esac ;;
curl)
  url= headers= header= write= body=/dev/stdout
  while (($#)); do
    case $1 in
      -D) headers=$2; shift ;;
      -o) body=$2; shift ;;
      -H) [[ $2 != @* ]] || header=$(cat "${2#@}"); shift ;;
      -w) write=1; shift ;;
      -d) shift ;;
      http*) url=$1 ;;
    esac
    shift
  done
  case $url in
    */readyz) ;;
    */metrics)
      echo 'gateway_requests_total{code="",model="demo-openai",status="200"} 1'
      if [[ $SCENARIO == local-limits ]]; then echo 'gateway_ratelimit_store_up 0'; else echo 'gateway_ratelimit_store_up 1'; fi ;;
    */v1/chat/completions)
      if [[ -n $write ]]; then
        [[ $header == 'Authorization: Bearer aws-limited-'* ]] || exit 2
        n=$(($(cat "$MOCK_DIR/limited" 2>/dev/null || echo 0) + 1))
        echo "$n" >"$MOCK_DIR/limited"
        case $SCENARIO in
          not-shared) admit=20 ;;
          refilling) admit=4 ;;
          *) admit=10 ;;
        esac
        if ((n <= admit)); then printf 404; else printf 429; fi
      else
        [[ $header == 'Authorization: Bearer aws-demo-'* ]] || exit 2
        id=AAAAAAAAAAAAAAAAAAAAAAAAAA
        [[ ! -f $MOCK_DIR/first ]] || id=BBBBBBBBBBBBBBBBBBBBBBBBBB
        touch "$MOCK_DIR/first"
        printf 'HTTP/1.1 200 OK\r\nX-Request-ID: %s\r\n\r\n' "$id" >"$headers"
        echo '{"choices":[{"message":{"content":"synthetic completion"}}]}' >"$body"
      fi ;;
    *) exit 1 ;;
  esac ;;
*) exit 1 ;;
esac
`
