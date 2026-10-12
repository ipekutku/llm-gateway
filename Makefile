# Developer and CI commands. CI runs `make check`'s targets as separate
# steps, so this file is the single list of verification commands.

GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.8.0

# Local PostgreSQL for development and the integration tests. CI uses the
# same image as a service. The port avoids a PostgreSQL already on 5432.
DB_CONTAINER := llm-gateway-postgres
DB_IMAGE := postgres:18
DB_PORT ?= 55432
DB_URL := postgres://gateway:gateway@127.0.0.1:$(DB_PORT)/gateway?sslmode=disable

# Local Redis for the integration tests, without persistence. CI uses the
# same image as a service. The port avoids a Redis already on 6379.
REDIS_CONTAINER := llm-gateway-redis
REDIS_IMAGE := redis:8
REDIS_PORT ?= 56379
REDIS_URL := redis://127.0.0.1:$(REDIS_PORT)/0

# Container images (see the Dockerfile). CI builds and scans both platforms on
# every run, and publishes them from main to PUBLISH_REGISTRY; manual private
# ECR publication is documented in docs/aws.md.
IMAGE ?= llm-gateway:dev
FAKEPROVIDER_IMAGE ?= llm-gateway-fakeprovider:dev
IMAGE_PLATFORMS := linux/amd64,linux/arm64
IMAGE_SOURCE := https://github.com/ipekutku/llm-gateway
PUBLISH_REGISTRY ?=
PUBLISH_TAG ?=
# The source label (and, for the multi-platform index, annotation) links a
# GHCR package to the repository; the revision is the commit built. Minimal
# provenance records the repository, commit, and pinned base images.
PUBLISH_FLAGS = --label org.opencontainers.image.source=$(IMAGE_SOURCE) \
	--annotation index:org.opencontainers.image.source=$(IMAGE_SOURCE) \
	--label org.opencontainers.image.revision=$$(git rev-parse HEAD) \
	--provenance=mode=min

# Image scanner, pinned by the digest of its multi-platform index (the same on
# GHCR, Docker Hub, and ECR Public). Update the tag and digest together:
#   docker buildx imagetools inspect ghcr.io/aquasecurity/trivy:<version>
# The vulnerability database is cached in a Docker volume between runs.
TRIVY_IMAGE := ghcr.io/aquasecurity/trivy:0.75.0@sha256:af6acf9a6b85dfe389a1941505c0ce9efef52a4719635e1a962f022a3d855daa
TRIVY_CACHE_VOLUME := llm-gateway-trivy-cache
TRIVY_FLAGS := --scanners vuln,secret --severity HIGH,CRITICAL --ignore-unfixed \
	--exit-code 1 --no-progress --disable-telemetry --skip-version-check

# Local Kubernetes in kind (see deploy/kind). kind runs pinned through go run;
# kubectl and helm must be on PATH. The cluster name, its kubectl context, the
# namespace, and the release name are also the defaults of deploy/kind/check.sh.
KIND := go run sigs.k8s.io/kind@v0.33.0
KIND_CLUSTER := llm-gateway
KIND_NAMESPACE := llm-gateway
KUBECTL_KIND := kubectl --context kind-$(KIND_CLUSTER) --namespace $(KIND_NAMESPACE)

# The AWS platform (see docs/aws.md): created for a working session, then
# destroyed. Needs AWS credentials (aws login), TF_VAR_aws_account_id, and
# TF_VAR_admin_cidr, the only address allowed to reach the Kubernetes API.
PLATFORM := deploy/aws/platform
AWS_CLUSTER := llm-gateway
AWS_KUBE_CONTEXT := llm-gateway-aws

# Local Prometheus, Grafana, and Jaeger for a gateway running on the host,
# and the loopback ports they are published on.
OBSERVABILITY := docker compose -f deploy/observability/compose.yaml
export PROMETHEUS_PORT ?= 9090
export GRAFANA_PORT ?= 3000
export JAEGER_UI_PORT ?= 16686
export OTLP_HTTP_PORT ?= 4318

.PHONY: check fmt vet test bench bench-check loadtest binary-size performance-probes build vuln image image-check image-scan image-publish helm-lint terraform-check aws-up aws-down aws-check-stores aws-deploy aws-check aws-leftovers aws-init kind-up kind-deploy kind-check kind-demo kind-down smoke db db-stop redis redis-stop observability observability-stop

## check: run every CI check
check: fmt vet test bench-check build vuln

## fmt: fail if any Go file is not gofmt-formatted
fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		printf 'Run gofmt on these files:\n%s\n' "$$unformatted"; \
		exit 1; \
	fi

## vet: go vet, including optional smoke/performance tests so they compile
vet:
	go vet ./...
	go vet -tags 'smoke performance' ./cmd/gateway

## test: unit and end-to-end tests against fake providers. The PostgreSQL
## integration tests run only when GATEWAY_TEST_DATABASE_URL is set (see db),
## and the Redis ones only when GATEWAY_TEST_REDIS_URL is set (see redis).
test:
	go test -race -timeout 2m ./...

## bench: measure request-path components locally, with allocation counts.
## The shared limiter benchmark needs GATEWAY_TEST_REDIS_URL (see redis).
bench:
	go test -run '^$$' -bench . -benchmem ./...

## bench-check: run each benchmark once for correctness, without timing gates.
bench-check:
	go test -run '^$$' -bench . -benchtime=1x ./...

## loadtest: synthetic local HTTP measurements; needs a migrated disposable
## GATEWAY_DATABASE_URL. Not a CI check; see docs/performance.md.
loadtest:
	bash deploy/loadtest/run.sh

## binary-size: local stripped build and module symbol attribution, not CI.
binary-size:
	bash deploy/loadtest/binary-size.sh

## performance-probes: local connection and Redis burst measurements, not CI.
## Redis needs GATEWAY_TEST_REDIS_URL; output is evidence, not a timing gate.
## LOADTEST_VEGETA_BIN optionally selects a completed load run's pinned tool.
performance-probes:
	go test -tags performance -count=1 -v -run '^Test(UpstreamConnectionReuse|InboundConnectionReuse|RedisPoolConcurrentMeasurement|VegetaInboundConnectionReuse)$$' ./cmd/gateway

## build: build both commands without leaving binaries behind
build:
	go build -o /dev/null ./cmd/gateway
	go build -o /dev/null ./cmd/fakeprovider

## vuln: known vulnerabilities reachable from the code (needs network)
vuln:
	go run $(GOVULNCHECK) ./...

## image: build the gateway and fake provider images for this machine's
## platform and load them into Docker as $(IMAGE) and $(FAKEPROVIDER_IMAGE)
image:
	docker build -t $(IMAGE) .
	docker build --target fakeprovider -t $(FAKEPROVIDER_IMAGE) .

## image-check: build both images for amd64 and arm64 without loading or
## pushing them; the results stay in the build cache. Needs a builder that
## supports several platforms (the containerd image store, or buildx's
## docker-container driver, as in CI).
image-check:
	docker buildx build --platform $(IMAGE_PLATFORMS) .
	docker buildx build --platform $(IMAGE_PLATFORMS) --target fakeprovider .

## image-scan: build each image for amd64 and arm64 and scan it with the
## pinned Trivy; fails on HIGH or CRITICAL vulnerabilities that have a fix,
## and on secrets. Each image reaches the scanner through a pipe, so it needs
## no bind mount or Docker socket. A failed build also fails the scan.
image-scan:
	@set -e; for platform in $$(echo '$(IMAGE_PLATFORMS)' | tr , ' '); do \
		for target in gateway fakeprovider; do \
			echo "Scanning $$target for $$platform"; \
			docker buildx build --progress quiet --platform $$platform --target $$target \
				--output type=docker,dest=- . | \
			docker run --rm -i -v $(TRIVY_CACHE_VOLUME):/root/.cache/trivy --entrypoint sh $(TRIVY_IMAGE) \
				-c 'cat >/tmp/image.tar && exec trivy image --input /tmp/image.tar $(TRIVY_FLAGS)'; \
		done; \
	done

## image-publish: build both images for amd64 and arm64 and push them as
## $(PUBLISH_REGISTRY)/llm-gateway[-fakeprovider]:$(PUBLISH_TAG). CI runs it
## on main after image-scan, logged in to the registry.
image-publish:
	@test -n '$(PUBLISH_REGISTRY)' && test -n '$(PUBLISH_TAG)' || \
		{ echo 'Set PUBLISH_REGISTRY and PUBLISH_TAG'; exit 1; }
	docker buildx build --platform $(IMAGE_PLATFORMS) --push $(PUBLISH_FLAGS) \
		--tag $(PUBLISH_REGISTRY)/llm-gateway:$(PUBLISH_TAG) .
	docker buildx build --platform $(IMAGE_PLATFORMS) --push $(PUBLISH_FLAGS) --target fakeprovider \
		--tag $(PUBLISH_REGISTRY)/llm-gateway-fakeprovider:$(PUBLISH_TAG) .

## helm-lint: lint and render the Helm chart with its CI values, and check
## that invalid values are rejected. Needs helm on PATH; not part of check.
helm-lint:
	bash deploy/helm/lint.sh

## terraform-check: format, initialize without a backend, validate, and run
## mocked plans. No AWS credentials; provider downloads need network access.
## Platform modules are downloaded again on every run: on some macOS 26 Macs a
## second init over installed local submodules hangs (hashicorp/terraform#39047).
terraform-check:
	terraform fmt -check -recursive deploy/aws
	terraform -chdir=deploy/aws/bootstrap init -backend=false -input=false -lockfile=readonly
	terraform -chdir=deploy/aws/bootstrap validate
	terraform -chdir=deploy/aws/bootstrap test
	rm -rf deploy/aws/platform/.terraform/modules
	terraform -chdir=deploy/aws/platform init -backend=false -input=false -lockfile=readonly
	terraform -chdir=deploy/aws/platform validate
	terraform -chdir=deploy/aws/platform test

## aws-up: create or update the platform (VPC, EKS, RDS, Valkey, secrets) after showing the
## plan for approval, then add the cluster to kubeconfig as llm-gateway-aws.
## Bills by the hour until aws-down; apply takes about 15-20 minutes.
aws-up: aws-init
	terraform -chdir=$(PLATFORM) apply
	aws eks update-kubeconfig --region eu-north-1 --name $(AWS_CLUSTER) --alias $(AWS_KUBE_CONTEXT)
	kubectl --context $(AWS_KUBE_CONTEXT) get nodes -o wide

## aws-check-stores: check private RDS and Valkey connections from a temporary
## pod, including TLS and authentication failures. Needs a running platform.
aws-check-stores:
	AWS_KUBE_CONTEXT=$(AWS_KUBE_CONTEXT) bash deploy/aws/check-stores.sh

## aws-deploy: install External Secrets Operator and the gateway chart on the
## platform, with the GHCR images of a commit on main (AWS_IMAGE_SHA, default
## the newest one this branch contains) and session-generated demo secrets.
## Needs a running platform; rerunning it upgrades in place.
aws-deploy:
	AWS_KUBE_CONTEXT=$(AWS_KUBE_CONTEXT) bash deploy/aws/deploy.sh

## aws-check: completions from both models, metrics, limits shared across two
## pods through Valkey, and usage records in RDS. Needs aws-deploy.
aws-check:
	AWS_KUBE_CONTEXT=$(AWS_KUBE_CONTEXT) bash deploy/aws/check.sh

## aws-down: destroy the platform after showing the plan for approval, and
## remove its kubeconfig entries. Bootstrap (state bucket, ECR) is kept.
aws-down: aws-init
	terraform -chdir=$(PLATFORM) destroy
	-kubectl config delete-context $(AWS_KUBE_CONTEXT)
	-kubectl config delete-cluster arn:aws:eks:eu-north-1:$(TF_VAR_aws_account_id):cluster/$(AWS_CLUSTER)
	-kubectl config delete-user arn:aws:eks:eu-north-1:$(TF_VAR_aws_account_id):cluster/$(AWS_CLUSTER)

## aws-leftovers: list anything in the Region that still bills (clusters,
## instances, volumes, NAT gateways, addresses, load balancers, databases);
## fails if anything is found. Run after every aws-down.
aws-leftovers:
	bash deploy/aws/leftovers.sh

# Initializes the platform against its S3 state. Modules are downloaded again
# each time (see terraform-check).
aws-init:
	@test -n '$(TF_VAR_aws_account_id)' && test -n '$(TF_VAR_admin_cidr)' || \
		{ echo 'Set TF_VAR_aws_account_id and TF_VAR_admin_cidr (see docs/aws.md)'; exit 1; }
	rm -rf $(PLATFORM)/.terraform/modules
	terraform -chdir=$(PLATFORM) init -input=false -lockfile=readonly -reconfigure \
		-backend-config='bucket=llm-gateway-tfstate-$(TF_VAR_aws_account_id)-eu-north-1' \
		-backend-config='allowed_account_ids=["$(TF_VAR_aws_account_id)"]'

## kind-up: create the local cluster if it does not exist, load both images,
## and start metrics-server and the disposable PostgreSQL and Redis
kind-up: image
	@$(KIND) get clusters | grep -qx '$(KIND_CLUSTER)' || \
		$(KIND) create cluster --name $(KIND_CLUSTER) --config deploy/kind/cluster.yaml --wait 2m
	$(KIND) load docker-image --name $(KIND_CLUSTER) $(IMAGE) $(FAKEPROVIDER_IMAGE)
	kubectl --context kind-$(KIND_CLUSTER) apply -k deploy/kind/metrics-server
	kubectl --context kind-$(KIND_CLUSTER) create namespace $(KIND_NAMESPACE) --dry-run=client -o yaml | \
		kubectl --context kind-$(KIND_CLUSTER) apply -f -
	$(KUBECTL_KIND) apply -f deploy/kind/stores.yaml
	$(KUBECTL_KIND) rollout status deployment/postgres deployment/redis --timeout=5m
	kubectl --context kind-$(KIND_CLUSTER) -n kube-system rollout status deployment/metrics-server --timeout=5m

## kind-deploy: create the synthetic Secrets and install or upgrade the chart
## with deploy/kind/values.yaml; the migration hook prepares the database
kind-deploy:
	@set -f; for secret in \
		'llm-gateway-database --from-literal=url=postgres://gateway:gateway@postgres:5432/gateway?sslmode=disable' \
		'llm-gateway-redis --from-literal=url=redis://redis:6379/0' \
		'llm-gateway-openai --from-literal=api-key=synthetic-openai-key' \
		'llm-gateway-anthropic --from-literal=api-key=synthetic-anthropic-key' \
		'llm-gateway-clients --from-file=clients.json=deploy/kind/clients.json'; do \
		$(KUBECTL_KIND) create secret generic $$secret --dry-run=client -o yaml | $(KUBECTL_KIND) apply -f - || exit 1; \
	done
	helm --kube-context kind-$(KIND_CLUSTER) upgrade --install llm-gateway deploy/helm/llm-gateway \
		--namespace $(KIND_NAMESPACE) -f deploy/kind/values.yaml --wait --timeout 5m

## kind-check: one chat request per provider through the Service, and the
## metrics port; needs kubectl and curl
kind-check:
	bash deploy/kind/check.sh

## kind-demo: demonstrate shared limits, shutdown, a rolling restart, a
## database outage, and autoscaling on the local cluster, after kind-up and
## kind-deploy (about ten minutes; results in deploy/kind/results, not CI)
kind-demo:
	bash deploy/kind/demo.sh

## kind-down: delete the local cluster and everything in it
kind-down:
	$(KIND) delete cluster --name $(KIND_CLUSTER)

## smoke: call the REAL provider APIs (costs money; never run in CI).
## Needs OPENAI_MODEL and OPENAI_API_KEY, ANTHROPIC_MODEL and
## ANTHROPIC_API_KEY, or both.
smoke:
	go test -tags smoke -count=1 -v -timeout 5m -run '^TestSmoke$$' ./cmd/gateway

## db: start a disposable local PostgreSQL in Docker; its data is deleted
## when it stops. Prints the variable that enables the integration tests.
db:
	docker run -d --rm --name $(DB_CONTAINER) \
		-e POSTGRES_USER=gateway -e POSTGRES_PASSWORD=gateway \
		-p 127.0.0.1:$(DB_PORT):5432 $(DB_IMAGE)
	@until docker exec $(DB_CONTAINER) pg_isready -h 127.0.0.1 -U gateway -q; do sleep 1; done
	@echo "export GATEWAY_TEST_DATABASE_URL='$(DB_URL)'"

## db-stop: stop the local PostgreSQL and delete its data
db-stop:
	docker stop $(DB_CONTAINER)

## redis: start a disposable local Redis in Docker, without persistence.
## Prints the variable that enables the Redis integration tests.
redis:
	docker run -d --rm --name $(REDIS_CONTAINER) \
		-p 127.0.0.1:$(REDIS_PORT):6379 $(REDIS_IMAGE) \
		redis-server --save '' --appendonly no
	@until docker exec $(REDIS_CONTAINER) redis-cli ping >/dev/null 2>&1; do sleep 1; done
	@echo "export GATEWAY_TEST_REDIS_URL='$(REDIS_URL)'"

## redis-stop: stop the local Redis and delete its data
redis-stop:
	docker stop $(REDIS_CONTAINER)

## observability: start Prometheus, Grafana, and Jaeger in Docker for a
## gateway running on the host (see the README). Not needed by CI.
observability:
	$(OBSERVABILITY) up -d --wait
	@echo "Grafana:    http://127.0.0.1:$(GRAFANA_PORT)"
	@echo "Prometheus: http://127.0.0.1:$(PROMETHEUS_PORT)"
	@echo "Jaeger:     http://127.0.0.1:$(JAEGER_UI_PORT)"
	@echo "export OTEL_EXPORTER_OTLP_ENDPOINT='http://127.0.0.1:$(OTLP_HTTP_PORT)'"

## observability-stop: stop the stack and delete its data
observability-stop:
	$(OBSERVABILITY) down -v
