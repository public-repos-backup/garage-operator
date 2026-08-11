package main

import (
	"bytes"
	"strings"
	"testing"
)

const (
	testSourceImage = "expected.invalid/app:latest"
	testPinnedImage = "expected.invalid/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func renderPinnedManifest(input string) (string, error) {
	var output bytes.Buffer
	err := pinManifest(strings.NewReader(input), &output, []string{testSourceImage + "=" + testPinnedImage})
	return output.String(), err
}

func TestPinManifestImagesStructuralYAMLForms(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		count int
	}{
		{name: "ordinary image", yaml: "spec:\n  image : " + testSourceImage + "\n", count: 1},
		{name: "flow image", yaml: "containers: [{name: sidecar, image: " + testSourceImage + "}]\n", count: 1},
		{name: "quoted image key", yaml: "spec:\n  \"image\": '" + testSourceImage + "'\n", count: 1},
		{name: "anchored args", yaml: "args: &runtimeArgs\n  - --image=" + testSourceImage + "\n", count: 1},
		{name: "aliased args", yaml: "runtimeArgs: &runtimeArgs\n  - --image=" + testSourceImage + "\nargs: *runtimeArgs\n", count: 1},
		{name: "folded shell args", yaml: "command: [sh, -c]\nargs:\n  - >-\n    runner --image=" + testSourceImage + "\n", count: 1},
		{name: "space form shell args", yaml: "command: [sh, -c]\nargs:\n  - runner --sidecar-image " + testSourceImage + "\n", count: 1},
		{name: "commented indentationless args", yaml: "args: # runtime flags\n- --image=" + testSourceImage + "\n", count: 1},
		{name: "split args", yaml: "args:\n  - --sidecar-image\n  - " + testSourceImage + "\n", count: 1},
		{name: "multiple flags", yaml: "args:\n  - runner --first-image=" + testSourceImage + " --second-image=" + testSourceImage + "\n", count: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, err := renderPinnedManifest(test.yaml)
			if err != nil {
				t.Fatal(err)
			}
			if count := strings.Count(output, testPinnedImage); count != test.count {
				t.Fatalf("pinned image count = %d, want %d\n%s", count, test.count, output)
			}
			if strings.Contains(output, testSourceImage) {
				t.Fatalf("source image remains in structural image position:\n%s", output)
			}
		})
	}
}

func TestPinManifestImagesIgnoresImageLookingDataAndNonTokenSubstrings(t *testing.T) {
	input := "data:\n  embedded.yaml: |\n    image: " + testSourceImage + "\nargs:\n  - --note=docs--image=" + testSourceImage + "\nspec:\n  image: " + testSourceImage + "\n"
	output, err := renderPinnedManifest(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(output, testSourceImage) != 2 {
		t.Fatalf("image-looking data or non-token argument was rewritten:\n%s", output)
	}
	if strings.Count(output, testPinnedImage) != 1 {
		t.Fatalf("real image field was not pinned:\n%s", output)
	}
}

func TestPinManifestImagesRejectsUnknownImagesInEveryStructuralForm(t *testing.T) {
	unknown := "unexpected.invalid/sidecar:latest"
	tests := []string{
		"spec:\n  image: " + testSourceImage + "\ncontainers: [image: " + unknown + "]\n",
		"spec:\n  image: " + testSourceImage + "\nargs: &runtimeArgs\n  - --image=" + unknown + "\n",
		"spec:\n  image: " + testSourceImage + "\nruntimeArgs: &runtimeArgs\n  - --image=" + unknown + "\nargs: *runtimeArgs\n",
		"spec:\n  image: " + testSourceImage + "\nargs:\n  - >-\n    runner --image=" + unknown + "\n",
		"spec:\n  image: " + testSourceImage + "\nargs:\n  - runner --sidecar-image " + unknown + "\n",
		"spec:\n  image: " + testSourceImage + "\nargs:\n  - --image\n  - " + unknown + "\n",
	}
	for _, input := range tests {
		if output, err := renderPinnedManifest(input); err == nil {
			t.Fatalf("unknown image was accepted:\n%s", output)
		}
	}
}

func TestPinManifestImagesFailureProducesNoOutput(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		arguments []string
	}{
		{
			name:      "unknown image",
			input:     "image: unexpected.invalid/app:latest\n",
			arguments: []string{testSourceImage + "=" + testPinnedImage},
		},
		{
			name:      "second document invalid",
			input:     "image: " + testSourceImage + "\n---\nimage: unexpected.invalid/app:latest\n",
			arguments: []string{testSourceImage + "=" + testPinnedImage},
		},
		{
			name:      "required mapping absent",
			input:     "kind: ConfigMap\n",
			arguments: []string{testSourceImage + "=" + testPinnedImage},
		},
		{
			name:      "quoted space form unsupported",
			input:     "args:\n  - runner --image '" + testSourceImage + "'\n",
			arguments: []string{testSourceImage + "=" + testPinnedImage},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := pinManifest(strings.NewReader(test.input), &output, test.arguments); err == nil {
				t.Fatalf("invalid manifest was accepted:\n%s", output.String())
			}
			if output.Len() != 0 {
				t.Fatalf("failure produced partial output:\n%s", output.String())
			}
		})
	}
}
