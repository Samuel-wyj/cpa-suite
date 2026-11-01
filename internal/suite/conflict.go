package suite

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Conflict is a plugin library that must not run next to CPA Suite.
type Conflict struct {
	PluginID string `json:"plugin_id"`
	Path     string `json:"path"`
	Reason   string `json:"reason"`
}

// Coexisting is a plugin library that was found and is allowed to run.
type Coexisting struct {
	PluginID string `json:"plugin_id"`
	Path     string `json:"path"`
	Note     string `json:"note"`
}

// pluginVersionPattern mirrors CLIProxyAPI's versioned file names
// (<id>-v<version>.<ext>), which the plugin store uses for installs.
var pluginVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*([-+][0-9A-Za-z.-]+)?$`)

// conflictingIDs run billing/admission/scheduling over the same requests and
// would double-charge or fight over credential selection.
var conflictingIDs = map[string]string{
	"cpa-key-billing": "与 CPA Suite 功能重复（计费、额度、并发、路由、调度），同时运行会重复扣费、调度冲突",
}

// coexistingIDs are recognised and intentionally allowed in this build.
var coexistingIDs = map[string]string{
	"client-guard": "client-guard.so 仍在加载：CPA Suite 的客户端限制模块已暂停（不记录、不拦截），由 client-guard 继续执行，避免重复 403。把 client-guard.so 移出 plugins 并重启 CPA 后，CPA Suite 自动只读导入它的规则与统计并接管",
}

// pluginIDFromFile follows CLIProxyAPI pluginhost.pluginFileFromPath.
func pluginIDFromFile(name string) (string, bool) {
	lower := strings.ToLower(name)
	ext := ""
	for _, candidate := range []string{".so", ".dylib", ".dll"} {
		if strings.HasSuffix(lower, candidate) {
			ext = candidate
		}
	}
	if ext == "" {
		return "", false
	}
	base := name[:len(name)-len(ext)]
	if i := strings.LastIndex(base, "-v"); i > 0 && pluginVersionPattern.MatchString(base[i+2:]) {
		base = base[:i]
	}
	return base, true
}

// ScanPlugins inspects the directories CLIProxyAPI loads plugins from:
// <dir>/<goos>/<goarch> and <dir>.
func ScanPlugins(dir string) ([]Conflict, []Coexisting) {
	var conflicts []Conflict
	var coexisting []Coexisting
	seen := map[string]bool{}
	for _, candidate := range []string{filepath.Join(dir, runtime.GOOS, runtime.GOARCH), dir} {
		entries, err := os.ReadDir(candidate)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			id, ok := pluginIDFromFile(entry.Name())
			if !ok {
				continue
			}
			path := filepath.Join(candidate, entry.Name())
			if seen[path] {
				continue
			}
			seen[path] = true
			if reason, bad := conflictingIDs[id]; bad {
				conflicts = append(conflicts, Conflict{PluginID: id, Path: path, Reason: reason})
			} else if note, ok := coexistingIDs[id]; ok {
				coexisting = append(coexisting, Coexisting{PluginID: id, Path: path, Note: note})
			}
		}
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	sort.Slice(coexisting, func(i, j int) bool { return coexisting[i].Path < coexisting[j].Path })
	return conflicts, coexisting
}
