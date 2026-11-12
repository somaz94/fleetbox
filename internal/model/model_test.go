package model

import (
	"testing"
	"time"
)

func TestParseServiceRef(t *testing.T) {
	tests := []struct {
		in      string
		want    ServiceRef
		wantErr bool
	}{
		{in: "prod/web/api", want: ServiceRef{Target: "prod", Scope: "web", Name: "api"}},
		{in: "prod/web", wantErr: true},
		{in: "prod//api", wantErr: true},
		{in: "a/b/c/d", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseServiceRef(tt.in)
		if (err != nil) != tt.wantErr {
			t.Fatalf("ParseServiceRef(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if err == nil && got != tt.want {
			t.Fatalf("ParseServiceRef(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
		if err == nil && got.String() != tt.in {
			t.Fatalf("round trip %q -> %q", tt.in, got.String())
		}
	}
}

func TestInstanceRefString(t *testing.T) {
	r := InstanceRef{Service: ServiceRef{Target: "t", Scope: "s", Name: "n"}, ID: "abc"}
	if got := r.String(); got != "t/s/n/abc" {
		t.Fatalf("got %q", got)
	}
}

func TestStopRecordOf(t *testing.T) {
	stopped := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ref := InstanceRef{Service: ServiceRef{Target: "t", Scope: "s", Name: "n"}, ID: "i"}

	tests := []struct {
		name     string
		in       Instance
		ok       bool
		wantExit *int
		wantImg  string
	}{
		{name: "running", in: Instance{Ref: ref, Status: StatusRunning}},
		{name: "stopped without time", in: Instance{Ref: ref, Status: StatusStopped}},
		{
			name: "non-zero exit wins over the first container",
			in: Instance{Ref: ref, Status: StatusStopped, StoppedAt: &stopped, StopReason: "Essential container exited",
				Containers: []Container{
					{Name: "sidecar", Image: "envoy", Digest: "sha256:a", ExitCode: Ptr(0)},
					{Name: "app", Image: "app", ExitCode: Ptr(137)},
				}},
			ok: true, wantExit: Ptr(137), wantImg: "envoy",
		},
		{
			name: "clean exit is kept",
			in: Instance{Ref: ref, Status: StatusStopped, StoppedAt: &stopped,
				Containers: []Container{{Name: "app", Image: "app", ExitCode: Ptr(0)}}},
			ok: true, wantExit: Ptr(0), wantImg: "app",
		},
		{
			name: "no exit code is absent",
			in:   Instance{Ref: ref, Status: StatusStopped, StoppedAt: &stopped, Containers: []Container{{Name: "app"}}},
			ok:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, ok := StopRecordOf(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if !rec.StoppedAt.Equal(stopped) || rec.Instance != ref || rec.Image != tt.wantImg {
				t.Fatalf("unexpected record %+v", rec)
			}
			switch {
			case tt.wantExit == nil && rec.ExitCode != nil:
				t.Fatalf("exit = %d, want absent", *rec.ExitCode)
			case tt.wantExit != nil && (rec.ExitCode == nil || *rec.ExitCode != *tt.wantExit):
				t.Fatalf("exit = %v, want %d", rec.ExitCode, *tt.wantExit)
			}
		})
	}
}
