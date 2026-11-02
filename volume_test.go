package main

import "testing"

func TestParseLocalVolume(t *testing.T) {
	for _, tc := range []struct {
		input string
		gain  float64
		ok    bool
	}{
		{"volume 0", 0, true}, {"volume 50", .5, true}, {"volume 100", 1, true},
		{"volume 101", 0, false}, {"volume -1", 0, false}, {"volume NaN", 0, false},
		{"volume 50 extra", 0, false}, {"stop", 0, false},
	} {
		gain, ok := parseLocalVolume(tc.input)
		if gain != tc.gain || ok != tc.ok {
			t.Errorf("%q => %v, %v", tc.input, gain, ok)
		}
	}
}

func TestParseMediaCommand(t *testing.T) {
	for _, tc := range []struct {
		input   string
		command uint64
		ok      bool
	}{
		{"media next", 4, true}, {"media previous", 5, true}, {"media toggle", 2, true},
		{"media skip", 0, false}, {"media next now", 0, false}, {"volume 5", 0, false},
	} {
		_, command, ok := parseMediaCommand(tc.input)
		if command != tc.command || ok != tc.ok {
			t.Errorf("%q => %v, %v", tc.input, command, ok)
		}
	}
}
