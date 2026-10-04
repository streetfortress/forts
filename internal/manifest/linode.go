package manifest

import "fmt"

// LinodeSpec is the [linode] table: the plan is a pricing decision made
// in the PR, never derived from [shape] (catalogs don't align).
//
//	[linode]
//	region = "us-sea"
//	type = "g6-standard-2"
type LinodeSpec struct {
	Region string `toml:"region"`
	Type   string `toml:"type"`
}

func (s *LinodeSpec) Validate(host string, h Host) error {
	if s.Region == "" || s.Type == "" {
		return fmt.Errorf("region and type are required")
	}
	return nil
}
