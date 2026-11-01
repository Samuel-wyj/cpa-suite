package plugin

import (
	"bytes"
	"path"
	"testing"
)

func TestUITemplateInlinesEveryWebAssetOnce(t *testing.T) {
	template, err := uiFiles.ReadFile("web/ui.html")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := uiFiles.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	assets := 0
	for _, entry := range entries {
		name := entry.Name()
		closing := map[string]string{".css": "</style", ".js": "</script"}[path.Ext(name)]
		if entry.IsDir() || closing == "" {
			continue
		}
		assets++
		if count := bytes.Count(template, []byte("/*"+name+"*/")); count != 1 {
			t.Errorf("%s is included %d times", name, count)
		}
		content, err := uiFiles.ReadFile("web/" + name)
		if err != nil {
			t.Fatal(err)
		}
		// Inlined assets must not close their own element early.
		if bytes.Contains(bytes.ToLower(content), []byte(closing)) {
			t.Errorf("%s contains %s", name, closing)
		}
	}
	if markers := len(uiInclude.FindAll(template, -1)); assets == 0 || markers != assets {
		t.Fatalf("template has %d include markers for %d assets", markers, assets)
	}
}
