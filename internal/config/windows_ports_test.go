package config

import "testing"

func TestWindowsPortsUsePerProjectOverride(t *testing.T) {
	cfg := Config{
		Net: NetCfg{WindowsPorts: []int{9420}},
		Project: map[string]ProjectCfg{
			"/example": {Net: NetCfg{WindowsPorts: []int{}}},
		},
	}
	project, err := cfg.PolicyFor("/example")
	if err != nil || len(project.WindowsPorts) != 0 {
		t.Fatalf("project policy: %+v, %v", project, err)
	}
	other, err := cfg.PolicyFor("/other")
	if err != nil || len(other.WindowsPorts) != 1 || other.WindowsPorts[0] != 9420 {
		t.Fatalf("default policy: %+v, %v", other, err)
	}
}

func TestWindowsPortsRejectAmbiguousOrInvalidPorts(t *testing.T) {
	for _, ports := range [][]int{{0}, {65536}, {9420, 9420}, {5432}} {
		cfg := Config{Net: NetCfg{HostPorts: []int{5432}, WindowsPorts: ports}}
		if _, err := cfg.PolicyFor("/example"); err == nil {
			t.Errorf("accepted Windows ports %v", ports)
		}
	}
}
