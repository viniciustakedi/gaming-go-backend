package domain_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

const modulePath = "github.com/viniciustakedi/jungle-gaming-wallet"

func TestDomainPackagesUseOnlyStandardLibraryAndDomainPackages(t *testing.T) {
	command := exec.Command("go", "list", "-json", "./internal/domain/...")
	command.Dir = "../.."
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list error = %v: %s", err, output)
	}

	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for decoder.More() {
		var pkg struct {
			ImportPath   string
			Imports      []string
			TestImports  []string
			XTestImports []string
		}
		if err := decoder.Decode(&pkg); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		for _, imports := range [][]string{pkg.Imports, pkg.TestImports, pkg.XTestImports} {
			for _, imported := range imports {
				if strings.Contains(imported, ".") && imported != modulePath+"/internal/domain" && !strings.HasPrefix(imported, modulePath+"/internal/domain/") {
					t.Errorf("%s imports non-domain dependency %q", pkg.ImportPath, imported)
				}
			}
		}
	}
}
