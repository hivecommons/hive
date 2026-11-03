package pushbroker

import (
	"path/filepath"
	"strings"
)

func ProtectedPathViolations(files, protected []string) []string {
	var rejected []string
	for _, file := range files {
		clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(file)), "./")
		for _, guard := range protected {
			g := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(guard)), "./")
			if strings.HasSuffix(guard, "/") || strings.HasSuffix(g, "/") {
				g = strings.TrimSuffix(g, "/") + "/"
				if strings.HasPrefix(clean, g) {
					rejected = append(rejected, clean)
					break
				}
				continue
			}
			if clean == g || strings.HasPrefix(clean, strings.TrimSuffix(g, "/")+"/") && strings.HasSuffix(guard, "/") {
				rejected = append(rejected, clean)
				break
			}
		}
	}
	return rejected
}
