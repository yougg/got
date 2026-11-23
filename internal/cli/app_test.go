package cli

import (
	"slices"
	"testing"
)

func TestParseGlobalFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantDir string
		wantOut []string
		wantErr bool
	}{
		{
			name:    "no flags",
			args:    []string{"tool", "vet"},
			wantDir: "",
			wantOut: []string{"tool", "vet"},
		},
		{
			name:    "space separated -C",
			args:    []string{"-C", "/tmp/proj", "tool", "stringer"},
			wantDir: "/tmp/proj",
			wantOut: []string{"tool", "stringer"},
		},
		{
			name:    "equals -C",
			args:    []string{"-C=/tmp/proj", "get", "-u"},
			wantDir: "/tmp/proj",
			wantOut: []string{"get", "-u"},
		},
		{
			name:    "missing value for -C",
			args:    []string{"-C"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, rest, err := ParseGlobalFlags(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dir != tc.wantDir || !slices.Equal(rest, tc.wantOut) {
				t.Errorf("got (%q, %v), want (%q, %v)", dir, rest, tc.wantDir, tc.wantOut)
			}
		})
	}
}

func TestExtractInitFlag(t *testing.T) {
	initFlag, rest := ExtractInitFlag([]string{"--init", "-tool", "example.com/a@v1.0.0"})
	if !initFlag {
		t.Error("expected autoInit=true")
	}
	want := []string{"-tool", "example.com/a@v1.0.0"}
	if !slices.Equal(rest, want) {
		t.Errorf("rest = %v, want %v", rest, want)
	}

	initFlag, rest = ExtractInitFlag([]string{"-tool", "example.com/a@v1.0.0"})
	if initFlag {
		t.Error("expected autoInit=false")
	}
	if !slices.Equal(rest, want) {
		t.Errorf("rest = %v, want %v", rest, want)
	}
}

func TestNormalizeGetArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "get -u without pkg appends tool",
			in:   []string{"-u"},
			want: []string{"-u", "tool"},
		},
		{
			name: "get -u=patch without pkg appends tool",
			in:   []string{"-u=patch"},
			want: []string{"-u=patch", "tool"},
		},
		{
			name: "get -u with pkg does not append tool",
			in:   []string{"-u", "example.com/a"},
			want: []string{"-u", "example.com/a"},
		},
		{
			name: "get -tool pkg unchanged",
			in:   []string{"-tool", "example.com/a@v1.0.0"},
			want: []string{"-tool", "example.com/a@v1.0.0"},
		},
		{
			name: "get -u with value flag only appends tool",
			in:   []string{"-u", "-tags", "dev"},
			want: []string{"-u", "-tags", "dev", "tool"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeGetArgs(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("NormalizeGetArgs(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseToolInvocation(t *testing.T) {
	inv := ParseToolInvocation([]string{"-n", "stringer", "-type=MyEnum"})
	if !inv.FlagN || inv.ToolName != "stringer" || !slices.Equal(inv.ToolArgs, []string{"-type=MyEnum"}) {
		t.Errorf("unexpected invocation: %+v", inv)
	}

	// 工具参数中的 -n 不应被当成 go tool -n
	inv2 := ParseToolInvocation([]string{"stringer", "-n", "-type=MyEnum"})
	if inv2.FlagN || inv2.ToolName != "stringer" || !slices.Equal(inv2.ToolArgs, []string{"-n", "-type=MyEnum"}) {
		t.Errorf("unexpected invocation2: %+v", inv2)
	}

	// 无参数列表模式
	inv3 := ParseToolInvocation(nil)
	if inv3.FlagN || inv3.ToolName != "" || len(inv3.ToolArgs) != 0 {
		t.Errorf("unexpected invocation3: %+v", inv3)
	}
}
