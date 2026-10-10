package evidence

// MigrationInstallationCollector proves only the reviewed pre-upgrade runtime
// boundary. An immutable image match remains mandatory; missing release labels
// are permitted only in this explicitly selected migration operation.
type MigrationInstallationCollector struct {
	*InstallationCollector
}

func NewMigrationInstallationCollector(r Runner, cfg InstallationConfig) (*MigrationInstallationCollector, error) {
	c, err := newInstallationCollector(r, cfg, true)
	if err != nil {
		return nil, err
	}
	return &MigrationInstallationCollector{InstallationCollector: c}, nil
}
