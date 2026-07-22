package visualhive

import (
	"reflect"
	"testing"
)

func TestResolveValidationCommandArgv(t *testing.T) {
	configured := [][]string{{"npm", "run", "test"}}
	tests := []struct {
		name     string
		required []string
		want     [][]string
		wantErr  bool
	}{
		{name: "installed argv", required: []string{"npm run test"}, want: [][]string{{"npm", "run", "test"}}},
		{name: "canonical exact head", required: []string{ExactHeadValidationCommand}, want: [][]string{{"git", "diff", "--check"}}},
		{name: "empty", wantErr: true},
		{name: "producer recipe", required: []string{"visual-hive improve-coverage && visual-hive issues --write"}, wantErr: true},
		{name: "canonical trailing space", required: []string{ExactHeadValidationCommand + " "}, wantErr: true},
		{name: "npx wrapper", required: []string{"npx " + ExactHeadValidationCommand}, wantErr: true},
		{name: "extra argument", required: []string{ExactHeadValidationCommand + " --config visual-hive.config.yaml"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveValidationCommandArgv(test.required, configured)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ResolveValidationCommandArgv(%q) did not fail closed", test.required)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("resolved argv = %#v, want %#v", got, test.want)
			}
		})
	}

	resolved, err := ResolveValidationCommandArgv([]string{"npm run test"}, configured)
	if err != nil {
		t.Fatal(err)
	}
	resolved[0][0] = "changed"
	if configured[0][0] != "npm" {
		t.Fatal("resolved argv aliases installed configuration")
	}
}

func TestVerifiedObservationValidationCommands(t *testing.T) {
	granular := "visual-hive mutate --config visual-hive.config.yaml --enforce-min-score"
	if got := verifiedObservationValidationCommands(granular); !reflect.DeepEqual(got, []string{ExactHeadValidationCommand}) {
		t.Fatalf("trusted Visual Hive guidance did not bind to exact-head validation: %v", got)
	}
	if got := verifiedObservationValidationCommands("npm --prefix AvProj run test:ui"); !reflect.DeepEqual(got, []string{"npm --prefix AvProj run test:ui"}) {
		t.Fatalf("repository validation argv was changed: %v", got)
	}
}
