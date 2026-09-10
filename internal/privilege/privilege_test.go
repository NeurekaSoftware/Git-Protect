package privilege

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Identity
		wantOK  bool
		wantErr bool
	}{
		{name: "neither set keeps current identity"},
		{name: "only user set is rejected", env: map[string]string{UserEnvVar: "1000"}, wantErr: true},
		{name: "only group set is rejected", env: map[string]string{GroupEnvVar: "1000"}, wantErr: true},
		{name: "empty user is rejected", env: map[string]string{UserEnvVar: "", GroupEnvVar: "1000"}, wantErr: true},
		{name: "empty group is rejected", env: map[string]string{UserEnvVar: "1000", GroupEnvVar: " "}, wantErr: true},
		{name: "non-numeric user is rejected", env: map[string]string{UserEnvVar: "root", GroupEnvVar: "1000"}, wantErr: true},
		{name: "non-numeric group is rejected", env: map[string]string{UserEnvVar: "1000", GroupEnvVar: "1000.5"}, wantErr: true},
		{name: "negative user is rejected", env: map[string]string{UserEnvVar: "-1", GroupEnvVar: "1000"}, wantErr: true},
		{name: "mixed root and non-root is rejected", env: map[string]string{UserEnvVar: "0", GroupEnvVar: "1000"}, wantErr: true},
		{name: "mixed non-root and root is rejected", env: map[string]string{UserEnvVar: "1000", GroupEnvVar: "0"}, wantErr: true},
		{name: "valid pair", env: map[string]string{UserEnvVar: "1000", GroupEnvVar: "1000"}, want: Identity{UID: 1000, GID: 1000}, wantOK: true},
		{name: "values are trimmed", env: map[string]string{UserEnvVar: " 1001 ", GroupEnvVar: "\t1002\n"}, want: Identity{UID: 1001, GID: 1002}, wantOK: true},
		{name: "root pair is allowed", env: map[string]string{UserEnvVar: "0", GroupEnvVar: "0"}, want: Identity{UID: 0, GID: 0}, wantOK: true},
		{name: "large ids are allowed", env: map[string]string{UserEnvVar: "65534", GroupEnvVar: "65534"}, want: Identity{UID: 65534, GID: 65534}, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				value, ok := tt.env[key]
				return value, ok
			}
			got, ok, err := parse(lookup)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parse() error = nil, want an error")
				}
				if got != (Identity{}) {
					t.Errorf("parse() identity = %+v, want zero identity on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse() error = %v, want nil", err)
			}
			if ok != tt.wantOK {
				t.Errorf("parse() ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("parse() identity = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestIdentityRoot(t *testing.T) {
	tests := []struct {
		name     string
		identity Identity
		want     bool
	}{
		{name: "root pair", identity: Identity{UID: 0, GID: 0}, want: true},
		{name: "non-root user", identity: Identity{UID: 1000, GID: 0}},
		{name: "non-root group", identity: Identity{UID: 0, GID: 1000}},
		{name: "non-root pair", identity: Identity{UID: 1000, GID: 1000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.identity.Root(); got != tt.want {
				t.Errorf("Root() = %v, want %v", got, tt.want)
			}
		})
	}
}
