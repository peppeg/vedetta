.PHONY: build run test test-offline clean eval eval-test judge smoke

build:
	@echo "Building Vedetta binary..."
	@mkdir -p bin
	go build -ldflags "-X main.version=$(shell cat VERSION)" -o bin/vedetta main.go

run: build
	@echo "Starting Vedetta on port 3030..."
	./bin/vedetta

test:
	@echo "Running tests..."
	go test -v ./...

# Level 1: unit tests with no network (only loopback, for httptest servers).
test-offline:
	@echo "Running tests without network..."
	unshare -rn sh -c 'ip link set lo up && go test -count=1 ./...'

clean:
	@rm -rf bin/

eval:
	@python3 scripts/eval.py $(if $(SESSION),--session $(SESSION),) $(if $(CASE),--case $(CASE),) $(ARGS)

eval-test:
	@python3 -m unittest scripts/test_eval.py scripts/test_judge.py scripts/test_bench_report.py scripts/test_ruler.py scripts/test_judge_links.py scripts/test_free_report.py scripts/test_score_known.py scripts/test_bench.py scripts/test_judge_quotes.py

# Level 3: live smoke test on the production service (3030), no judge.
smoke:
	@python3 scripts/eval.py --all --suite smoke --pause 120 --log scripts/out/smoke.log

judge:
	@python3 scripts/judge.py $(SESSION) $(ARGS)
