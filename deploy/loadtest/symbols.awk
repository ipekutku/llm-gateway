# Longest package prefix assigns text symbols to a linked package's module. Symbols
# with no unique package prefix (itab/generics/linker metadata) stay unassigned.
# This is symbol attribution, not a measurement of removable file bytes.
FNR == NR { owners[$1] = $2; next }
NF >= 4 && $2 ~ /^[0-9]+$/ && $3 ~ /^[Tt]$/ && !seen[$1]++ {
    name = $4
    sub(/^type:\.eq\./, "", name)
    sub(/^type:\.hash\./, "", name)
    owner = "unassigned"; longest = 0
    for (pkg in owners) {
        if (index(name, pkg ".") == 1 && length(pkg) > longest) {
            owner = owners[pkg]; longest = length(pkg)
        }
    }
    sizes[owner] += $2
}
END { for (owner in sizes) printf "%.0f %s\n", sizes[owner], owner }
