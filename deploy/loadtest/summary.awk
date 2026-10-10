# Only reads numeric fields from the pinned Vegeta JSON report and JSON logs.
# Full reports and raw scrapes remain the source of truth, not this summary.
function number(text, key,    pattern, value) {
    pattern = "\"" key "\"[[:space:]]*:[[:space:]]*[-+0-9.eE]+"
    if (!match(text, pattern)) {
        print "Missing numeric report field: " key > "/dev/stderr"
        exit 1
    }
    value = substr(text, RSTART, RLENGTH)
    sub(/^[^:]*:[[:space:]]*/, "", value)
    return value + 0
}
function report(file,    line, data) {
    while ((getline line < file) > 0) data = data line
    close(file)
    if (data == "") {
        print "Missing report: " file > "/dev/stderr"
        exit 1
    }
    return data
}
function metric(file, name,    line, fields, value, found) {
    while ((getline line < file) > 0) {
        split(line, fields, " ")
        if (fields[1] == name) { value = fields[2] + 0; found = 1 }
    }
    close(file)
    if (!found) {
        print "Missing metric: " name > "/dev/stderr"
        exit 1
    }
    return value
}
function row(name, data) {
    printf "| %s | %.2f | %.3f | %.3f | %.3f | %.4f%% | %.0f |\n", name,
        number(data, "throughput"), number(data, "50th") / 1e6,
        number(data, "95th") / 1e6, number(data, "99th") / 1e6,
        100 * (1 - number(data, "success")), number(data, "requests")
}
function count200(data) {
    # Vegeta omits status-code map entries that never occurred.
    if (!match(data, /"200"[[:space:]]*:/)) return 0
    return number(data, "200")
}
BEGIN {
    if (mode == "rate") {
        printf "%.0f\n", number(report(file), "throughput") * 0.7
        exit
    }
    baseline = report(dir "/baseline.json")
    gateway = report(dir "/gateway.json")
    print "| Path | Successful RPS | p50 ms | p95 ms | p99 ms | Errors | Requests |"
    print "|---|---:|---:|---:|---:|---:|---:|"
    row("Direct fake", baseline)
    row("Gateway", gateway)
    printf "\nPercentile differences (gateway minus direct): p50 %.3f, p95 %.3f, p99 %.3f ms.\n",
        (number(gateway, "50th") - number(baseline, "50th")) / 1e6,
        (number(gateway, "95th") - number(baseline, "95th")) / 1e6,
        (number(gateway, "99th") - number(baseline, "99th")) / 1e6

    before = dir "/before.prom"; after = dir "/after.prom"
    cpu = metric(after, "process_cpu_seconds_total") - metric(before, "process_cpu_seconds_total")
    gc = metric(after, "go_gc_duration_seconds_count") - metric(before, "go_gc_duration_seconds_count")
    pauses = metric(after, "go_gc_duration_seconds_sum") - metric(before, "go_gc_duration_seconds_sum")
    alloc = metric(after, "go_memstats_alloc_bytes_total") - metric(before, "go_memstats_alloc_bytes_total")
    if (cpu < 0 || gc < 0 || pauses < 0 || alloc < 0) {
        print "Metric counter decreased; measurement is invalid" > "/dev/stderr"
        exit 1
    }
    while ((getline line < (dir "/samples.prom")) > 0) {
        split(line, fields, " ")
        if (fields[1] == "process_resident_memory_bytes" && fields[2] + 0 > rss) rss = fields[2] + 0
        if (fields[1] == "go_goroutines" && fields[2] + 0 > goroutines) goroutines = fields[2] + 0
    }
    close(dir "/samples.prom")
    if (rss <= 0 || goroutines <= 0) {
        print "Missing memory/goroutine samples" > "/dev/stderr"
        exit 1
    }
    while ((getline line < (dir "/gateway.log")) > 0) {
        if (index(line, "\"msg\":\"usage records dropped: queue full\"")) drops += number(line, "count")
        if (index(line, "\"msg\":\"usage records not persisted\"")) lost += number(line, "count")
    }
    close(dir "/gateway.log")
    printf "\nGateway CPU: %.3f core-seconds; sampled peak RSS: %.2f MiB; peak goroutines: %.0f.\n", cpu, rss / 1048576, goroutines
    printf "GC: %.0f cycles, %.3f ms total pauses; allocated: %.2f MiB.\n", gc, pauses * 1000, alloc / 1048576
    printf "Queue-full drops: %.0f; failed-batch records: %.0f (process total, includes warmup and shutdown).\n", drops, lost
    print "RSS/goroutines sampled about once per second; CPU/GC/allocation deltas cover the metric-sampling window."
    print "Percentile differences are independent distributions, not per-request overhead or a capacity guarantee."
    # Saturation may produce real HTTP errors. Retain them visibly rather than
    # hiding the scaling limit or mistaking successful RPS for attempted RPS.
    if (number(baseline, "success") != 1 || number(gateway, "success") != 1 ||
        count200(baseline) != number(baseline, "requests") ||
        count200(gateway) != number(gateway, "requests"))
        print "WARNING: HTTP errors/non-200 responses occurred; this is not an error-free capacity result."
    if (rate > 0) {
        actual = number(gateway, "rate")
        direct_rate = number(baseline, "rate")
        printf "Open target: %.0f RPS; issued: direct %.2f, gateway %.2f RPS.\n", rate, direct_rate, actual
        if (actual < rate * 0.95 || actual > rate * 1.05 ||
            direct_rate < rate * 0.95 || direct_rate > rate * 1.05) {
            print "Open arrival rate missed by more than 5%; do not interpret as an open-model result."
            exit 1
        }
    }
}
