package cli

import (
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type profile struct {
	Port     *int    `toml:"port"`
	User     *string `toml:"user"`
	Password *string `toml:"password"`
	Database *string `toml:"database"`
}

type configuration struct {
	DefaultProfile *string            `toml:"default_profile"`
	Profiles       map[string]profile `toml:"profiles"`
}

func readConfig(who identity) (configuration, error) {
	var config configuration
	f, err := openConfig(who)
	if err != nil {
		return config, err
	}
	defer f.Close()
	if err := toml.NewDecoder(f).DisallowUnknownFields().Decode(&config); err != nil {
		return configuration{}, fmt.Errorf("invalid TOML configuration")
	}
	if config.DefaultProfile == nil {
		defaultName := "dev"
		config.DefaultProfile = &defaultName
	}
	if strings.TrimSpace(*config.DefaultProfile) == "" || len(config.Profiles) == 0 {
		return configuration{}, fmt.Errorf("configuration requires named profiles and a nonempty default profile")
	}
	for name, p := range config.Profiles {
		if p.Port == nil || p.User == nil || p.Password == nil || p.Database == nil {
			return configuration{}, fmt.Errorf("profile requires port, user, password, and database")
		}
		if strings.TrimSpace(name) == "" || *p.Port < 1 || *p.Port > 65535 || strings.TrimSpace(*p.User) == "" || strings.TrimSpace(*p.Database) == "" {
			return configuration{}, fmt.Errorf("profile has an invalid name, port, user, or database")
		}
	}
	if _, ok := config.Profiles[*config.DefaultProfile]; !ok {
		return configuration{}, fmt.Errorf("default profile does not exist")
	}
	return config, nil
}
