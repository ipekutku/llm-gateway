package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestAWSDemo(t *testing.T) {
	failures := map[string]string{
		"missing-key": "load-key is missing",
		"drain-fails": "drain of node-a failed",
	}
	for _, tc := range []struct{ scenario, args string }{
		{"missing-key", "database"},
		{"database", "database"},
		{"drain-fails", "drain"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			dir := t.TempDir()
			session := filepath.Join(dir, "session")
			if err := os.Mkdir(session, 0700); err != nil {
				t.Fatal(err)
			}
			key := "aws-load-" + strings.Repeat("k", 48)
			if tc.scenario != "missing-key" {
				if err := os.WriteFile(filepath.Join(session, "load-key"), []byte(key), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err, mockDir := awsScriptMocks(t, "deploy/aws/demo.sh", []string{tc.args}, awsDemoMock,
				[]string{"go", "sleep"}, "SCENARIO="+tc.scenario, "AWS_SESSION_DIR="+session)
			if (err == nil) != (tc.scenario == "database") || !strings.Contains(out, failures[tc.scenario]) {
				t.Fatalf("script result: %v\n%s", err, out)
			}
			calls := readMockFile(t, mockDir, "calls")
			if strings.Contains(out, key) || strings.Contains(calls, key) {
				t.Fatal("the load client's key appeared in output or command arguments")
			}
			// The key may only be in the temporary target file, never in the results.
			results, _ := filepath.Glob(filepath.Join(session, "demo-*", "*"))
			for _, path := range results {
				if data, _ := os.ReadFile(path); strings.Contains(string(data), key) {
					t.Fatalf("%s contains the load client's key", path)
				}
			}

			switch tc.scenario {
			case "missing-key":
				if strings.Contains(calls, "kubectl") || strings.Contains(calls, "aws ") {
					t.Fatal("touched the cluster or AWS without a load key")
				}
			case "drain-fails":
				if !strings.Contains(calls, "kubectl --context llm-gateway-aws uncordon node-a") {
					t.Fatalf("the drained node was not uncordoned:\n%s", calls)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(readMockFile(t, mockDir, "vegeta.pid")))
				if err != nil {
					t.Fatal(err)
				}
				if syscall.Kill(pid, 0) == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatal("the load generator kept running after the script failed")
				}
			case "database":
				for _, want := range []string{
					"rds reboot-db-instance --region eu-north-1 --db-instance-identifier llm-gateway ",
					"delete pod llm-gateway-demo-usage",
				} {
					if !strings.Contains(calls, want) {
						t.Fatalf("missing call %q:\n%s", want, calls)
					}
				}
				for _, want := range []string{
					"- requests: 4800 at 20/s, success 100%, status codes 200: 4800",
					"- gateway logs: 2 `usage records not persisted` warnings for 7 records",
					"- usage rows stored for this scenario's client and time: 4790",
					"- accounted for: 4797 of 4800 requests",
				} {
					if !strings.Contains(out, want) {
						t.Fatalf("missing summary line %q:\n%s", want, out)
					}
				}
				pod := readMockFile(t, mockDir, "usage-pod.yaml")
				if !strings.Contains(pod, "WHERE client_id = 'load' AND received_at >= '20") ||
					!strings.Contains(pod, "value: verify-full") {
					t.Fatalf("usage count pod:\n%s", pod)
				}
			}
		})
	}
}

// go install leaves a vegeta that is this mock; its attack runs until killed
// in drain-fails and ends at once otherwise. sleep returns at once. RDS
// reports rebooting once, then available; the gateway pods logged two lost
// batches of 3 and 4 records, and the usage table holds 4790 rows.
const awsDemoMock = `#!/usr/bin/env bash
set -eu
echo "$(basename "$0") $*" >>"$MOCK_DIR/calls"
case $(basename "$0") in
sleep) ;;
go) mkdir -p "$GOBIN"; ln -s "$MOCK_DIR/mock" "$GOBIN/vegeta" ;;
vegeta)
  case $1 in
    attack)
      if [[ $SCENARIO == drain-fails ]]; then echo $$ >"$MOCK_DIR/vegeta.pid"; exec /bin/sleep 30; fi ;;
    report)
      if [[ " $* " == *' -type=json '* ]]; then
        echo '{"requests":4800,"rate":20.0,"success":1,"status_codes":{"200":4800},"latencies":{"50th":250000000,"99th":300000000,"max":400000000}}'
      else
        echo 'text report'
      fi ;;
  esac ;;
git) [[ $1 == rev-parse ]] && echo 0123abc || true ;;
curl) ;;
terraform) [[ ${@: -1} == database_identifier ]] && echo llm-gateway ;;
aws)
  case "$1 $2" in
    'rds reboot-db-instance') echo rebooting ;;
    'rds describe-db-instances')
      if [[ -f $MOCK_DIR/rebooted ]]; then echo available; else touch "$MOCK_DIR/rebooted"; echo rebooting; fi ;;
    *) exit 1 ;;
  esac ;;
kubectl)
  while [[ $1 == --* ]]; do shift 2; done
  case "$1 $2" in
    'rollout status') ;;
    'get deployment')
      case $* in
        *image*) echo ghcr.io/ipekutku/llm-gateway:mock ;;
        *) echo 2 ;;
      esac ;;
    'get nodes') echo '{"items":[{"metadata":{"name":"node-a","labels":{"node.kubernetes.io/instance-type":"c7i-flex.large","topology.kubernetes.io/zone":"eu-north-1a"}},"status":{"nodeInfo":{"kubeletVersion":"v1.37.0"}}}]}' ;;
    'get pods') echo '{"items":[
      {"metadata":{"name":"gateway-a"},"spec":{"nodeName":"node-a"},"status":{"phase":"Running"}},
      {"metadata":{"name":"gateway-b"},"spec":{"nodeName":"node-a"},"status":{"phase":"Running"}},
      {"metadata":{"name":"gateway-c"},"spec":{"nodeName":"node-b"},"status":{"phase":"Running"}}]}' ;;
    'drain node-a') [[ $SCENARIO != drain-fails ]] ;;
    'uncordon node-a') ;;
    'logs --selector')
      echo '{"level":"WARN","msg":"usage records not persisted","count":3,"error":"timeout"}'
      echo '{"level":"INFO","msg":"request completed","status":200}'
      echo '{"level":"WARN","msg":"usage records not persisted","count":4,"error":"timeout"}' ;;
    'delete pod') ;;
    'apply -f') cat >"$MOCK_DIR/usage-pod.yaml" ;;
    'get pod') echo Succeeded ;;
    'logs llm-gateway-demo-usage') echo 4790 ;;
    *) exit 1 ;;
  esac ;;
*) exit 1 ;;
esac
`
