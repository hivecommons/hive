package config

// ResolveKickLogSource returns the kick log source in force: the env override
// when valid, else the configured value when valid, else scrollback.
func (l LoggingConfig) ResolveKickLogSource() string {
	source, _ := l.resolveKickLogSource()
	return source
}

// KickLogSourceOrigin reports where ResolveKickLogSource's answer came from
// (KickLogSourceOriginEnv, KickLogSourceOriginConfig or
// KickLogSourceOriginDefault) so the dashboard can say when the environment is
// overriding the setting.
func (l LoggingConfig) KickLogSourceOrigin() string {
	_, origin := l.resolveKickLogSource()
	return origin
}

func (l LoggingConfig) resolveKickLogSource() (source, origin string) {
	if v := KickLogSourceEnvOverride(); v != "" {
		return v, KickLogSourceOriginEnv
	}
	if ValidKickLogSource(l.KickLogSource) {
		v := NormalizeKickLogSource(l.KickLogSource)
		return v, KickLogSourceOriginConfig
	}
	return KickLogSourceScrollback, KickLogSourceOriginDefault
}
