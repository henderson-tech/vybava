package hostsetup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGradleFreshFile(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	if err := Apply(home, false, &out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(gradlePath(home))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != gradleKey+"="+gradleValue+"\n" {
		t.Errorf("unexpected file:\n%s", raw)
	}
	if !strings.HasPrefix(out.String(), "applied ") {
		t.Errorf("unexpected report: %s", out.String())
	}
}

func TestGradleEditsInPlace(t *testing.T) {
	home := t.TempDir()
	path := gradlePath(home)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	original := "# my gradle\norg.gradle.jvmargs=-Xmx4g\n" + gradleKey + " = 10800000\norg.gradle.parallel=true\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Apply(home, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	want := "# my gradle\norg.gradle.jvmargs=-Xmx4g\n" + gradleKey + "=" + gradleValue + "\norg.gradle.parallel=true\n"
	if string(raw) != want {
		t.Errorf("got:\n%s\nwant:\n%s", raw, want)
	}
}

func TestGradleAlreadyRightIsNoop(t *testing.T) {
	home := t.TempDir()
	path := gradlePath(home)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("org.gradle.jvmargs=-Xmx4g\n"+gradleKey+"="+gradleValue+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Apply(home, false, &out); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if !info.ModTime().Equal(old) {
		t.Error("file was rewritten although the key already held")
	}
	if !strings.HasPrefix(out.String(), "ok ") {
		t.Errorf("unexpected report: %s", out.String())
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	if err := Apply(home, true, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gradlePath(home)); !os.IsNotExist(err) {
		t.Error("dry run must not create the file")
	}
	if !strings.HasPrefix(out.String(), "would ") {
		t.Errorf("unexpected report: %s", out.String())
	}
}
