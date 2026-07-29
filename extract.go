package main

import ("fmt";"os";"regexp";"strings")

func main() {
	b, _ := os.ReadFile("pkg/hub/saas.go")
	s := string(b)
	var out []string
	re := regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	fmt.Fprintln(os.Stderr, "blocks:", len(out))
	os.WriteFile("/tmp/hive-alerts/dash.js", []byte(strings.Join(out, "\n;\n")), 0644)
}
