package config

import (
	"bufio"
	"os"
	"strings"
)

// LoadDotEnv reads a .env style file and puts every pair into the process
// environment. Values already present in the environment win, so a real env var
// always beats the file — the usual precedence, and the one that lets a container
// override a baked-in default.
//
// A missing file is not an error: the binary is expected to run from plain
// environment variables in a container.
func LoadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		key, value, ok := parseDotEnvLine(scanner.Text())
		if !ok {
			continue
		}

		if _, exists := os.LookupEnv(key); exists {
			continue
		}

		_ = os.Setenv(key, value)
	}

	return scanner.Err()
}

func parseDotEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)

	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}

	line = strings.TrimPrefix(line, "export ")

	name, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", false
	}

	return name, unquote(strings.TrimSpace(value)), true
}

// unquote strips one matching pair of quotes. An unquoted value keeps everything
// up to the first unescaped '#', so a trailing comment does not end up in the
// value — quoted values keep '#' verbatim.
func unquote(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]

		if first == last && (first == '"' || first == '\'') {
			inner := value[1 : len(value)-1]

			if first == '"' {
				inner = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(inner)
			}

			return inner
		}
	}

	if hash := strings.Index(value, " #"); hash >= 0 {
		value = value[:hash]
	}

	return strings.TrimSpace(value)
}
