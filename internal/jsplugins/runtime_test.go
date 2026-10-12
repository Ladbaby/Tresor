package jsplugins

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, source string) *Package {
	t.Helper()
	pkg, err := LoadPackage(map[string]string{"manifest.json": `{"id":"test/fixture","version":"1.0.0","api_version":2,"entry":"plugin.js","requires":[],"permissions":[],"config_schema":{"type":"object"}}`, "plugin.js": source})
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}
func TestLoggingPackagesRejected(t *testing.T) {
	for _, tc := range []struct{ name, extra, source string }{
		{"feature", `,"requires":["log.v1"]`, `module.exports={}`},
		{"permission", `,"permissions":["log.v1"]`, `module.exports={}`},
		{"import", "", `const log=require('tresor/log.v1');module.exports={request(){log.info('message')}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"manifest.json": `{"id":"test/logging","version":"1.0.0","api_version":2,"entry":"plugin.js"` + tc.extra + `}`, "plugin.js": tc.source}
			if _, err := LoadPackage(files); err == nil {
				t.Fatal("logging package accepted")
			}
			pkg, err := loadPackage(files, true)
			if err != nil {
				t.Fatal(err)
			}
			if pkg.Compatible || len(pkg.programs) != 0 {
				t.Fatal("cached logging package executable")
			}
		})
	}
}

func TestPackageIDs(t *testing.T) {
	for _, id := range []string{"a", "new_plugin", "my-plugin2", "break_repeat", "custom_header", "acme/plugin", "a-b/c_d2"} {
		t.Run(id, func(t *testing.T) {
			if !ValidPackageID(id) {
				t.Fatal("valid ID rejected")
			}
			manager, err := NewManager(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			files := map[string]string{"manifest.json": fmt.Sprintf(`{"id":%q,"version":"1.0.0","api_version":2,"entry":"plugin.js"}`, id), "plugin.js": `module.exports={}`}
			if _, err := manager.Install(files); err != nil {
				t.Fatal(err)
			}
			if err := manager.Activate(id, "1.0.0"); err != nil {
				t.Fatal(err)
			}
			if manager.Snapshot()[id] == nil {
				t.Fatal("package not active")
			}
		})
	}
	for _, id := range []string{"", "A", "1name", "_name", "-name", "ns/", "/name", "a/b/c", "a/1b", "a/B", "a.b", "a\\b", "a b", "a\n"} {
		if ValidPackageID(id) {
			t.Errorf("invalid ID accepted: %q", id)
		}
	}
	manager, err := NewManager(t.TempDir(), []string{"native"})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"manifest.json": `{"id":"native","version":"1.0.0","api_version":2,"entry":"plugin.js"}`, "plugin.js": `module.exports={}`}
	if _, err := LoadPackage(files); err != nil {
		t.Fatal("native ID should be syntactically valid:", err)
	}
	if _, err := manager.Install(files); err == nil {
		t.Fatal("native ID collision accepted")
	}
}

func TestRuntimeIsolationAndValidation(t *testing.T) {
	pkg := fixture(t, `let globalCount=0;module.exports={request({state,body}){state.count=(state.count||0)+1;globalCount++;return {body:{encoding:'utf8',data:String(state.count)+':'+globalCount}}}}`)
	instance, err := NewInstance(pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []string{"1:1", "2:2", "1:1"} {
		if index == 2 {
			if err := instance.Reset(); err != nil {
				t.Fatal(err)
			}
		}
		out, err := instance.Invoke("request", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		if out["body"].(map[string]any)["data"] != want {
			t.Fatal(out)
		}
	}
}
func TestRuntimeRejectsFailures(t *testing.T) {
	for _, source := range []string{`module.exports={typo(){}}`, `require('fs');module.exports={}`, `while(true){}`, `module.exports={request:1}`} {
		if _, err := LoadPackage(map[string]string{"manifest.json": `{"id":"test/failure","version":"1.0.0","api_version":2,"entry":"plugin.js"}`, "plugin.js": source}); err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
	for _, result := range []string{`null`, `{routing:'evil'}`, `{then(){}}`, `(()=>{while(true){}})()`, `{get body(){while(true){}}}`} {
		pkg := fixture(t, fmt.Sprintf(`module.exports={request(){return %s}}`, result))
		instance, err := NewInstance(pkg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := instance.Invoke("request", map[string]any{}); err == nil {
			t.Fatalf("accepted %s", result)
		}
	}
}
func TestInitializationExceptionSanitized(t *testing.T) {
	_, err := LoadPackage(map[string]string{"manifest.json": `{"id":"test/error","version":"1.0.0","api_version":2,"entry":"plugin.js"}`, "plugin.js": `throw new Error('SECRET_PAYLOAD')`})
	if err == nil || strings.Contains(err.Error(), "SECRET_PAYLOAD") {
		t.Fatalf("unsafe initialization error: %v", err)
	}
}

func TestLegacyConfigCacheBlockedAndDeletable(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"manifest.json": `{"id":"test/legacy","version":"1.0.0","api_version":2,"entry":"plugin.js","config_schema":{"type":"object","properties":{"secret":{"type":"string"}}}}`, "plugin.js": `throw new Error('must not evaluate')`}
	if _, err := m.Install(files); err == nil {
		t.Fatal("configurable install accepted")
	}
	digest, err := SourceDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "packages", digest+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	m, err = NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.List()) != 1 || m.List()[0].Executable {
		t.Fatal("legacy package not blocked")
	}
	if err := m.SetEnabled("test/legacy", "1.0.0", true); err == nil {
		t.Fatal("blocked enable accepted")
	}
	if err := m.Delete("test/legacy", "1.0.0"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerDurableAndImmutable(t *testing.T) {
	dir := t.TempDir()
	manager, err := NewManager(dir, []string{"native"})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"manifest.json": `{"id":"test/installed","version":"1.0.0","api_version":2,"entry":"plugin.js"}`, "plugin.js": `module.exports={request(){}}`}
	if _, err := manager.Install(files); err != nil {
		t.Fatal(err)
	}
	if len(manager.Snapshot()) != 1 {
		t.Fatal("install did not enable plugin")
	}
	if err := manager.Activate("test/installed", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	restored, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Snapshot()) != 1 {
		t.Fatal("lost selection")
	}
	files["plugin.js"] = `module.exports={response(){}}`
	if _, err := manager.Install(files); err == nil {
		t.Fatal("mutable version")
	}
	if err := manager.Activate("test/installed", "missing"); err == nil {
		t.Fatal("missing activation")
	}
	os.Remove(filepath.Join(dir, "active.json"))
	os.Mkdir(filepath.Join(dir, "active.json"), 0700)
	if err := manager.Activate("test/installed", "1.0.0"); err == nil {
		t.Fatal("persist failure accepted")
	}
	if len(manager.Snapshot()) != 1 {
		t.Fatal("failed activation changed state")
	}
}
func TestLosslessLibraryPatch(t *testing.T) {
	pkg := fixture(t, `const j=require('tresor/json.v1');module.exports={request(){const raw='{"big":9007199254740993,"decimal":1.234567890123456789,"array":[1,2]}';const out=j.patch(raw,[{op:'add',path:'/array/-',value:'tail'},{op:'replace',path:'/new',value:1}]);return {body:{encoding:'utf8',data:out}}}}`)
	instance, _ := NewInstance(pkg, nil)
	if _, err := instance.Invoke("request", map[string]any{}); err == nil {
		t.Fatal("replace missing key")
	}
}
