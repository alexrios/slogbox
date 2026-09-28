#!/usr/bin/env bash

set -euo pipefail

bench_runs=${BENCH_RUNS:-10}
bench_time=${BENCH_TIME:-1s}
bench_gomaxprocs=${BENCH_GOMAXPROCS:-1}
bench_cpu=${BENCH_CPU:-}

if ! [[ $bench_runs =~ ^[1-9][0-9]*$ ]]; then
	printf 'BENCH_RUNS must be a positive integer\n' >&2
	exit 2
fi
if ! [[ $bench_gomaxprocs =~ ^[1-9][0-9]*$ ]]; then
	printf 'BENCH_GOMAXPROCS must be a positive integer\n' >&2
	exit 2
fi

bench_results_dir=$(mktemp -d)
trap 'rm -rf "$bench_results_dir"' EXIT
v1_results=$bench_results_dir/v1.txt
v2_results=$bench_results_dir/v2.txt
benchmark_pattern='^Benchmark(JSON|WriteTo)$'

affinity=()
if [[ -n $bench_cpu ]]; then
	if ! command -v taskset >/dev/null 2>&1; then
		printf 'BENCH_CPU requires taskset\n' >&2
		exit 2
	fi
	affinity=(taskset -c "$bench_cpu")
fi

run_mode() {
	local mode=$1
	local destination=$2
	local experiment=(GOEXPERIMENT=jsonv2)
	if [[ $mode == v1 ]]; then
		experiment=(GOEXPERIMENT=nojsonv2)
	fi

	"${affinity[@]}" env "GOMAXPROCS=$bench_gomaxprocs" "${experiment[@]}" \
		mise exec -- go test \
		-run='^$' \
		-bench="$benchmark_pattern" \
		-benchmem \
		-benchtime="$bench_time" \
		-count=1 \
		-cpu="$bench_gomaxprocs" \
		./... >>"$destination"
}

for ((run = 1; run <= bench_runs; run++)); do
	printf 'benchmark pair %d/%d\n' "$run" "$bench_runs" >&2
	if ((run % 2 == 1)); then
		run_mode v2 "$v2_results"
		run_mode v1 "$v1_results"
	else
		run_mode v1 "$v1_results"
		run_mode v2 "$v2_results"
	fi
done

mise exec -- benchstat "v1=$v1_results" "v2=$v2_results"
