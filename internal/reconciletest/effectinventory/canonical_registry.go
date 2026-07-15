package effectinventory

// CanonicalRegistry assembles the closed production boundary vocabulary and
// every classified store, provider, process, and event site. It is the sole
// registry accepted by the owning reconciler inventory gate.
func CanonicalRegistry() (Registry, error) {
	partitions := []func() ([]SiteRegistration, error){
		storeInventoryRegistrations,
		providerInventoryRegistrations,
		processInventoryRegistrations,
		eventInventoryRegistrations,
	}
	var registrations []SiteRegistration
	for _, partition := range partitions {
		sites, err := partition()
		if err != nil {
			return Registry{}, err
		}
		registrations = append(registrations, sites...)
	}
	return Registry{
		Boundaries:    CanonicalBoundaries(),
		Registrations: registrations,
	}, nil
}
