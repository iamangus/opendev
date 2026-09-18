package foundation

import (
	"strings"
	"testing"
)

func TestFilesDefinePinnedCIContract(t *testing.T) {
	files := Files()
	if !strings.Contains(files[ManifestPath], "version: "+Version) {
		t.Fatalf("manifest does not declare foundation version: %q", files[ManifestPath])
	}
	workflow := files[WorkflowPath]
	if !strings.Contains(workflow, "iamangus/opendev/.github/workflows/opendev-ci.yml@v4") {
		t.Fatalf("workflow is not pinned to the reusable CI contract: %q", workflow)
	}
	if !strings.Contains(workflow, "contents: read") {
		t.Fatalf("workflow does not restrict permissions: %q", workflow)
	}
}
