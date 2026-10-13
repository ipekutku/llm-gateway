package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestAWSIngress(t *testing.T) {
	for _, tc := range []struct {
		action, scenario, failure string
	}{
		{"preflight", "issued", ""},
		{"preflight", "pending", "not ISSUED"},
		{"preflight", "wrong-account", "different account"},
		{"preflight", "no-zone", "hosted zone is missing"},
		{"prepare", "issued", ""},
		{"prepare", "bad-chart", "checksum mismatch"},
		{"publish", "fresh", ""},
		{"publish", "gated", ""},
		{"publish", "missing-gates", "lack ALB readiness gates"},
		{"publish", "foreign-lb", "unexpected ownership tags"},
		{"publish", "foreign-record", "unexpected A/AAAA record"},
		{"publish", "api-fails", "Could not inspect"},
		{"down", "issued", ""},
		{"down", "no-alb", ""},
		{"down", "pending", ""},
		{"down", "foreign-lb", "unexpected ownership tags"},
		{"down", "foreign-record", "unexpected A/AAAA record"},
		{"down", "api-fails", "Could not inspect"},
		{"records", "records-clean", ""},
		{"records", "issued", ""},
	} {
		t.Run(tc.action+"/"+tc.scenario, func(t *testing.T) {
			out, err, dir := awsScriptArgs(t, "deploy/aws/ingress.sh", []string{tc.action}, awsIngressMock, "SCENARIO="+tc.scenario)
			if (err != nil) != (tc.failure != "") || (tc.failure != "" && !strings.Contains(out, tc.failure)) {
				t.Fatalf("script result: %v\n%s", err, out)
			}
			calls := readMockFile(t, dir, "calls")
			if tc.failure != "" {
				if strings.Contains(calls, "change-resource-record-sets") || strings.Contains(calls, "delete ingress") {
					t.Fatalf("mutated DNS or deleted an Ingress after a failed check:\n%s", calls)
				}
				if tc.action == "preflight" || tc.scenario == "bad-chart" {
					if strings.Contains(calls, "helm ") || strings.Contains(calls, "kubectl ") {
						t.Fatal("preflight/checksum failure must precede cluster mutation")
					}
				}
				return
			}
			switch tc.action {
			case "prepare":
				if !strings.Contains(calls, "--set vpcId=vpc-0123456789abcdef0") ||
					!strings.Contains(calls, "label namespace llm-gateway elbv2.k8s.aws/pod-readiness-gate-inject=enabled") {
					t.Fatalf("missing explicit VPC or readiness label:\n%s", calls)
				}
			case "publish":
				binding, restart := strings.Index(calls, "get targetgroupbindings"), strings.Index(calls, "rollout restart")
				if tc.scenario == "fresh" && (restart < binding || binding < 0) {
					t.Fatal("first pods must be replaced only after the target-group binding exists")
				}
				if tc.scenario == "gated" && restart >= 0 {
					t.Fatal("already gated pods should not restart again")
				}
				if i, j := strings.Index(calls, "rollout status"), strings.Index(calls, "change-resource-record-sets"); i < 0 || j < i {
					t.Fatal("DNS was published before the gated rollout completed")
				}
				ingress := readMockFile(t, dir, "ingress.yaml")
				for _, want := range []string{"host: llm-gateway.utkuipek.dev", "inbound-cidrs: 203.0.113.10/32", "certificate-arn: arn:aws:acm:eu-north-1:123456789012:certificate/mock"} {
					if !strings.Contains(ingress, want) || strings.Contains(ingress, "__") {
						t.Fatalf("incorrect ingress substitution:\n%s", ingress)
					}
				}
				var change struct {
					Changes []struct {
						Action            string
						ResourceRecordSet struct {
							Name, Type  string
							AliasTarget struct{ DNSName, HostedZoneId string }
						}
					}
				}
				if err := json.Unmarshal([]byte(readMockFile(t, dir, "dns.json")), &change); err != nil {
					t.Fatal(err)
				}
				if len(change.Changes) != 1 || change.Changes[0].Action != "UPSERT" ||
					change.Changes[0].ResourceRecordSet.Name != "llm-gateway.utkuipek.dev." ||
					change.Changes[0].ResourceRecordSet.Type != "A" ||
					change.Changes[0].ResourceRecordSet.AliasTarget.DNSName != "dualstack.llm-gateway-123.eu-north-1.elb.amazonaws.com." ||
					change.Changes[0].ResourceRecordSet.AliasTarget.HostedZoneId != "ZALB" {
					t.Fatalf("incorrect session alias: %+v", change)
				}
			case "down":
				if strings.Contains(calls, "describe-certificate") {
					t.Fatal("teardown cannot depend on certificate status")
				}
				if tc.scenario != "no-alb" {
					i, j := strings.Index(calls, "change-resource-record-sets"), strings.Index(calls, "delete ingress")
					if i < 0 || j < i || !strings.Contains(calls[j:], "describe-load-balancers") ||
						!strings.Contains(readMockFile(t, dir, "dns.json"), `"Action": "DELETE"`) {
						t.Fatal("teardown must remove the alias, then Ingress, then verify AWS cleanup")
					}
				}
			case "records":
				var records []any
				if err := json.Unmarshal([]byte(out), &records); err != nil {
					t.Fatal(err)
				}
				want := 1
				if tc.scenario == "records-clean" {
					want = 0
				}
				if len(records) != want {
					t.Fatalf("got %d leftover records, want %d: %s", len(records), want, out)
				}
			}
		})
	}
}

func TestAWSIngressDrainSettings(t *testing.T) {
	ingress, err := os.ReadFile("../../deploy/aws/ingress.yaml")
	if err != nil {
		t.Fatal(err)
	}
	values, err := os.ReadFile("../../deploy/aws/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	readInt := func(text, prefix string) int {
		t.Helper()
		for _, line := range strings.Split(text, "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
				n, err := strconv.Atoi(value)
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
		}
		t.Fatalf("missing %s", prefix)
		return 0
	}
	delay := readInt(string(values), "delaySeconds: ")
	drain := readInt(string(values), "timeoutSeconds: ")
	grace := readInt(string(values), "terminationGracePeriodSeconds: ")
	deregistration := readInt(string(ingress), "alb.ingress.kubernetes.io/target-group-attributes: deregistration_delay.timeout_seconds=")
	if delay <= deregistration || drain < deregistration || grace < delay+drain+10 {
		t.Fatalf("incompatible ALB/shutdown budgets: deregistration=%d delay=%d drain=%d grace=%d", deregistration, delay, drain, grace)
	}
	for _, want := range []string{`listen-ports: '[{"HTTPS":443}]'`, "ssl-policy: ELBSecurityPolicy-TLS13-1-2-Res-2021-06", "healthcheck-path: /readyz", "target-type: ip", "number: 8080"} {
		if !strings.Contains(string(ingress), want) {
			t.Errorf("missing ingress security/routing setting %s", want)
		}
	}
}

func TestAWSIngressHTTPS(t *testing.T) {
	for _, scenario := range []string{"success", "tls-fails", "auth-open", "metrics-exposed", "unexpected-completion"} {
		t.Run(scenario, func(t *testing.T) {
			session := t.TempDir()
			key := "synthetic-private-ingress-key"
			if err := os.WriteFile(session+"/demo-key", []byte(key), 0600); err != nil {
				t.Fatal(err)
			}
			out, err, dir := awsScript(t, "deploy/aws/check-ingress.sh", awsIngressHTTPSMock,
				"SCENARIO="+scenario, "AWS_SESSION_DIR="+session)
			if (err == nil) != (scenario == "success") {
				t.Fatalf("script result: %v\n%s", err, out)
			}
			calls := readMockFile(t, dir, "calls")
			if strings.Contains(out, key) || strings.Contains(calls, key) {
				t.Fatal("client key appeared in output or command arguments")
			}
			if strings.Contains(calls, "--insecure") || strings.Contains(calls, " -k") || strings.Contains(calls, "http://") {
				t.Fatal("HTTPS check bypassed TLS verification")
			}
			for _, call := range strings.Split(strings.TrimSpace(calls), "\n") {
				if !strings.Contains(call, "--connect-timeout 10 --max-time 30") {
					t.Fatalf("unbounded curl call: %s", call)
				}
			}
			if scenario == "success" && (!strings.Contains(calls, "/metrics") || !strings.Contains(calls, "/debug/pprof/")) {
				t.Fatal("sensitive API surfaces were not checked")
			}
		})
	}
}

const awsIngressHTTPSMock = `#!/usr/bin/env bash
set -eu
echo "$(basename "$0") $*" >>"$MOCK_DIR/calls"
[[ $(basename "$0") == curl ]] || exit 1
[[ $SCENARIO != tls-fails ]] || exit 60
url= header=
while (($#)); do
  case $1 in
    https://*) url=$1 ;;
    -H) [[ $2 != @* ]] || header=$(<"${2#@}"); shift ;;
  esac
  shift
done
case $url in
  */readyz) echo ok ;;
  */v1/chat/completions)
    if [[ -z $header ]]; then
      if [[ $SCENARIO == auth-open ]]; then printf 200; else printf 401; fi
    else
      [[ $header == 'Authorization: Bearer synthetic-private-ingress-key' ]] || exit 2
      if [[ $SCENARIO == unexpected-completion ]]; then echo '{"choices":[{"message":{"content":"other"}}]}'
      else echo '{"choices":[{"message":{"content":"synthetic completion"}}]}'; fi
    fi ;;
  */metrics|*/debug/pprof/)
    if [[ $SCENARIO == metrics-exposed ]]; then printf 200; else printf 404; fi ;;
  *) exit 1 ;;
esac
`

const awsIngressMock = `#!/usr/bin/env bash
set -eu
echo "$(basename "$0") $*" >>"$MOCK_DIR/calls"
case $(basename "$0") in
aws)
  case $2 in
    get-caller-identity)
      if [[ $SCENARIO == wrong-account ]]; then echo '{"Account":"999999999999"}'; else echo '{"Account":"123456789012"}'; fi ;;
    list-hosted-zones-by-name)
      if [[ $SCENARIO == no-zone ]]; then echo '{"HostedZones":[]}'; else echo '{"HostedZones":[{"Id":"/hostedzone/ZMOCK","Name":"llm-gateway.utkuipek.dev.","Config":{"PrivateZone":false}}]}'; fi ;;
    list-certificates) echo '{"CertificateSummaryList":[{"DomainName":"llm-gateway.utkuipek.dev","CertificateArn":"arn:aws:acm:eu-north-1:123456789012:certificate/mock"}]}' ;;
    describe-certificate)
      status=ISSUED
      [[ $SCENARIO != pending ]] || status=PENDING_VALIDATION
      printf '{"Certificate":{"DomainName":"llm-gateway.utkuipek.dev","Status":"%s","CertificateArn":"arn:aws:acm:eu-north-1:123456789012:certificate/mock","DomainValidationOptions":[{"DomainName":"llm-gateway.utkuipek.dev","ResourceRecord":{"Name":"_token.llm-gateway.utkuipek.dev","Type":"CNAME","Value":"_token.acm-validations.aws"}}]}}' "$status" ;;
    describe-load-balancers)
      [[ $SCENARIO != api-fails ]] || exit 2
      if [[ $SCENARIO == no-alb || -f $MOCK_DIR/deleted ]]; then echo '{"LoadBalancers":[]}'
      else echo '{"LoadBalancers":[{"LoadBalancerName":"llm-gateway","LoadBalancerArn":"arn:aws:elasticloadbalancing:eu-north-1:123456789012:loadbalancer/app/llm-gateway/mock","DNSName":"llm-gateway-123.eu-north-1.elb.amazonaws.com","CanonicalHostedZoneId":"ZALB"}]}'; fi ;;
    describe-tags)
      project=llm-gateway
      [[ $SCENARIO != foreign-lb ]] || project=other
      printf '{"TagDescriptions":[{"Tags":[{"Key":"Project","Value":"%s"},{"Key":"Stack","Value":"platform"},{"Key":"elbv2.k8s.aws/cluster","Value":"llm-gateway"},{"Key":"ingress.k8s.aws/stack","Value":"llm-gateway/llm-gateway"}]}]}' "$project" ;;
    list-resource-record-sets)
      printf '{"ResourceRecordSets":[{"Name":"llm-gateway.utkuipek.dev.","Type":"NS"},{"Name":"llm-gateway.utkuipek.dev.","Type":"SOA"},{"Name":"_token.llm-gateway.utkuipek.dev.","Type":"CNAME","ResourceRecords":[{"Value":"_token.acm-validations.aws."}]}'
      if [[ $SCENARIO == foreign-record ]]; then printf ',{"Name":"llm-gateway.utkuipek.dev.","Type":"A","ResourceRecords":[{"Value":"192.0.2.1"}]}'
      elif [[ $SCENARIO != records-clean && $SCENARIO != no-alb ]]; then printf ',{"Name":"llm-gateway.utkuipek.dev.","Type":"A","AliasTarget":{"DNSName":"dualstack.llm-gateway-123.eu-north-1.elb.amazonaws.com.","HostedZoneId":"ZALB","EvaluateTargetHealth":false}}'; fi
      echo ']}' ;;
    change-resource-record-sets)
      while (($#)); do
        if [[ $1 == --change-batch ]]; then cp "${2#file://}" "$MOCK_DIR/dns.json"; break; fi
        shift
      done
      echo '{"ChangeInfo":{"Id":"/change/mock"}}' ;;
    wait) ;;
    list-clusters) if [[ $SCENARIO == no-alb ]]; then echo '{"clusters":[]}'; else echo '{"clusters":["llm-gateway"]}'; fi ;;
    describe-security-groups) echo '{"SecurityGroups":[]}' ;;
    *) exit 1 ;;
  esac ;;
terraform)
  case ${@: -1} in
    vpc_id) echo vpc-0123456789abcdef0 ;;
    admin_cidr) echo 203.0.113.10/32 ;;
    *) exit 1 ;;
  esac ;;
curl)
  while (($#)); do
    if [[ $1 == -o ]]; then printf fixture >"$2"; break; fi
    shift
  done ;;
shasum)
  if [[ $SCENARIO == bad-chart ]]; then echo 'different'; else echo 'ee0403fcd713d6f1d28b1ff17d0d6d47e011cc4dd727a0aed0d95ae46897226a fixture'; fi ;;
helm) ;;
kubectl)
  while [[ $1 == --* ]]; do shift 2; done
  case "$1 $2" in
    'label namespace'|'rollout status') ;;
    'apply -f') cp "$3" "$MOCK_DIR/ingress.yaml" ;;
    'get targetgroupbindings') echo '{"items":[{"spec":{"serviceRef":{"name":"llm-gateway"},"targetType":"ip"}}]}' ;;
    'rollout restart') touch "$MOCK_DIR/restarted" ;;
    'get pods')
      if [[ $SCENARIO == missing-gates || ($SCENARIO == fresh && ! -f $MOCK_DIR/restarted) ]]; then echo '{"items":[{"spec":{}},{"spec":{}}]}'
      else echo '{"items":[{"spec":{"readinessGates":[{"conditionType":"target-health.elbv2.k8s.aws/mock"}]}},{"spec":{"readinessGates":[{"conditionType":"target-health.elbv2.k8s.aws/mock"}]}}]}'; fi ;;
    'get ingress')
      if [[ $* == *jsonpath* ]]; then echo llm-gateway-123.eu-north-1.elb.amazonaws.com
      else echo '{"spec":{"rules":[{"host":"llm-gateway.utkuipek.dev"}]},"metadata":{"annotations":{"alb.ingress.kubernetes.io/load-balancer-name":"llm-gateway"}}}'; fi ;;
    'delete ingress') touch "$MOCK_DIR/deleted" ;;
    *) exit 1 ;;
  esac ;;
*) exit 1 ;;
esac
`
