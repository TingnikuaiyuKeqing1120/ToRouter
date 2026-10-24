package store

// ConfigurationError identifies a known validation refusal, distinct from a
// database/network error whose mutation outcome may be uncertain.
type ConfigurationError struct{ Message string }

func (e *ConfigurationError) Error() string      { return e.Message }
func NewConfigurationError(message string) error { return &ConfigurationError{Message: message} }
