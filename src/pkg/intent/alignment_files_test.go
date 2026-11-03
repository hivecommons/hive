package intent

import (
	"reflect"
	"testing"
)

func TestAlignmentFileSummaries(t *testing.T) {
	files := make([]ChangedFile, MaxAlignmentFiles+1)
	want := make([]DiffFileSummary, len(files))
	for i := range files {
		files[i] = ChangedFile{
			Filename: "src/example.go", Status: "modified",
			Additions: i + 1, Deletions: i + 2,
			Patch: "@@ -1 +1 @@\n-old\n+new",
		}
		want[i] = DiffFileSummary{
			Filename: "src/example.go", Status: "modified",
			Additions: i + 1, Deletions: i + 2,
		}
	}

	if got := allFiles(files); !reflect.DeepEqual(got, want) {
		t.Fatalf("allFiles() = %v, want %v", got, want)
	}
	bounded, truncated := boundedFiles(files)
	if !reflect.DeepEqual(bounded, want[:MaxAlignmentFiles]) || truncated != 1 {
		t.Fatalf("boundedFiles() = %v, %d; want %v, 1", bounded, truncated, want[:MaxAlignmentFiles])
	}
	bounded, truncated = boundedFiles(files[:1])
	if !reflect.DeepEqual(bounded, want[:1]) || truncated != 0 {
		t.Fatalf("boundedFiles(single file) = %v, %d; want %v, 0", bounded, truncated, want[:1])
	}
}
