package session

// Clone detaches mutable settings values at publication boundaries.
func (s Settings) Clone() Settings {
	copy := s
	if s.Temperature != nil {
		value := *s.Temperature
		copy.Temperature = &value
	}
	if s.MaxOutputTokens != nil {
		value := *s.MaxOutputTokens
		copy.MaxOutputTokens = &value
	}
	copy.AlwaysAllow = append([]string(nil), s.AlwaysAllow...)
	copy.AlwaysDeny = append([]string(nil), s.AlwaysDeny...)
	copy.AdditionalDirectories = append([]string(nil), s.AdditionalDirectories...)
	copy.AdditionalReadOnlyDirectories = append([]string(nil), s.AdditionalReadOnlyDirectories...)
	copy.DisallowedDirectories = append([]string(nil), s.DisallowedDirectories...)
	return copy
}
