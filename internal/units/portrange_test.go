package units

import "testing"

func TestListenPort(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "host and port", in: "0.0.0.0:9090", want: "9090"},
		{name: "loopback", in: "127.0.0.1:8080", want: "8080"},
		{name: "bare colon", in: ":8080", want: "8080"},
		{name: "ipv6", in: "[::1]:9090", want: "9090"},
		{name: "no colon falls back", in: "8080", want: "8080"},
		{name: "malformed falls back", in: "garbage", want: "8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ListenPort(tt.in); got != tt.want {
				t.Errorf("ListenPort(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParsePortRange(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantFirst  int
		wantLast   int
		wantErr    bool
	}{
		{name: "valid", in: "8090-8099", wantFirst: 8090, wantLast: 8099},
		{name: "whitespace", in: "  8090 - 8099  ", wantFirst: 8090, wantLast: 8099},
		{name: "single port", in: "8080-8080", wantFirst: 8080, wantLast: 8080},
		{name: "reversed", in: "9000-8000", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "no dash", in: "8090", wantErr: true},
		{name: "non-numeric start", in: "abc-8099", wantErr: true},
		{name: "non-numeric end", in: "8090-def", wantErr: true},
		{name: "zero start", in: "0-100", wantErr: true},
		{name: "too large", in: "8090-70000", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, last, err := ParsePortRange(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParsePortRange(%q) expected error, got (%d, %d)",
						tt.in, first, last)
				}
				return
			}
			if err != nil {
				t.Errorf("ParsePortRange(%q) unexpected error: %v", tt.in, err)
				return
			}
			if first != tt.wantFirst || last != tt.wantLast {
				t.Errorf("ParsePortRange(%q) = (%d, %d), want (%d, %d)",
					tt.in, first, last, tt.wantFirst, tt.wantLast)
			}
		})
	}
}
