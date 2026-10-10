package auth

import "testing"

func BenchmarkAuthenticate(b *testing.B) {
	const valid = "synthetic-benchmark-key-0000000000000000000000"
	a, err := New([]Client{{ID: "bench", KeyHash: HashKey(valid)}})
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		key  string
		want error
	}{
		{"valid", valid, nil},
		{"unknown", "synthetic-benchmark-key-1111111111111111111111", ErrInvalidKey},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				id, err := a.Authenticate(tc.key)
				if err != tc.want || (err == nil && id.ClientID != "bench") {
					b.Fatalf("Authenticate: identity %v, error %v", id, err)
				}
			}
		})
	}
}
