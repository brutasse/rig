package maven

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Server struct {
	Username string
	Password string
}

type Settings struct {
	LocalRepo string
	Servers   map[string]Server
}

func DefaultUserSettings() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".m2", "settings.xml")
}

// Credentials returns the server credentials for id from the default user
// settings.xml ("", "" when the file or the server is absent).
func Credentials(id string) (string, string) {
	p := DefaultUserSettings()
	if p == "" {
		return "", ""
	}
	st, err := Load(p)
	if err != nil {
		return "", ""
	}
	if s, ok := st.Servers[id]; ok {
		return s.Username, s.Password
	}
	return "", ""
}

// Load parses <localRepository> and the <servers> section of a Maven
// settings.xml (with or without the Maven XML namespace) into a Settings
// value. A missing file yields zero settings, not an error.
func Load(path string) (Settings, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Settings{Servers: map[string]Server{}}, nil
		}
		return Settings{}, err
	}
	defer f.Close()
	s := Settings{Servers: map[string]Server{}}
	var (
		inServers, inServer bool
		id, user, pass      string
		field               *string
	)
	dec := xml.NewDecoder(f)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Settings{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "localRepository":
				field = &s.LocalRepo
			case "servers":
				inServers = true
			case "server":
				if inServers {
					inServer = true
					id, user, pass = "", "", ""
				}
			case "id":
				if inServer {
					field = &id
				}
			case "username":
				if inServer {
					field = &user
				}
			case "password":
				if inServer {
					field = &pass
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "localRepository":
				field = nil
			case "id", "username", "password":
				field = nil
			case "server":
				if inServer && id != "" {
					s.Servers[id] = Server{Username: user, Password: pass}
				}
				inServer = false
			case "servers":
				inServers = false
			}
		case xml.CharData:
			if field != nil {
				*field = strings.TrimSpace(string(t))
			}
		}
	}
	return s, nil
}
