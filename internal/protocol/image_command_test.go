package protocol

import "testing"

func TestImageOnlyInputNormalizesAndOwnsBytes(t *testing.T) {
	img := testInputPNG(t)
	cmd := NewSubmitInput("command", "session", "input", "", InputSteer)
	cmd.Input.Images = []InputImage{img}
	normalized, err := cmd.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	img.Data[0] = 0
	if normalized.Input.Images[0].Data[0] == 0 {
		t.Fatal("normalized image is mutable by caller")
	}
	cmd.Input.Images = nil
	if _, err := cmd.Normalize(); err == nil {
		t.Fatal("empty input accepted")
	}
}
