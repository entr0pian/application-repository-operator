/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package yamlpatch

import (
	"strings"
	"testing"
)

func TestDeletePresentKey(t *testing.T) {
	doc, err := Parse([]byte("repositories:\n  backend:\n    imageTag: abc123\n    namespace: default\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if changed := doc.Delete("repositories", "backend", "imageTag"); !changed {
		t.Fatal("Delete on a present key returned false, want true")
	}

	out, err := doc.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if strings.Contains(string(out), "imageTag") {
		t.Fatalf("imageTag key still present after Delete: %s", out)
	}
	if !strings.Contains(string(out), "namespace: default") {
		t.Fatalf("sibling key namespace was lost after Delete: %s", out)
	}
}

func TestDeleteAbsentKey(t *testing.T) {
	doc, err := Parse([]byte("repositories:\n  backend:\n    namespace: default\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if changed := doc.Delete("repositories", "backend", "imageTag"); changed {
		t.Fatal("Delete on an absent key returned true, want false")
	}
}

func TestDeleteMissingParentPath(t *testing.T) {
	doc, err := Parse([]byte("repositories:\n  backend:\n    namespace: default\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if changed := doc.Delete("repositories", "frontend", "imageTag"); changed {
		t.Fatal("Delete under a non-existent parent path returned true, want false")
	}
}

func TestDeleteOnEmptyDocument(t *testing.T) {
	doc, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if changed := doc.Delete("repositories", "backend", "imageTag"); changed {
		t.Fatal("Delete on an empty document returned true, want false")
	}
}
