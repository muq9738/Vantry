package provider

// KnownID validates supported execution providers independently of model vendors.
func KnownID(id string) bool {
	return id == "pi" || id == "controlled"
}
