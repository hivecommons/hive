package knowledge

import "testing"

func TestValidateGitSourceURL_RejectsPrivateHosts(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	blocked := []string{
		"http://127.0.0.1:8080/repo.git",
		"http://127.255.255.255/repo.git",
		"http://10.0.0.1/internal-repo.git",
		"http://169.254.169.254/latest/meta-data/",
		"http://169.254.0.1/latest/meta-data/",
		"http://192.168.1.1/sensitive-path",
		"http://172.16.0.1/repo.git",
		"http://172.31.255.255/repo.git",
		"https://localhost/repo.git",
		"http://[::1]/repo.git",
		"http://[::ffff:127.0.0.1]/repo.git",
		"http://[::ffff:169.254.169.254]/repo.git",
		"http://[fc00::1]/repo.git",
		"http://[fd12::1]/repo.git",
		"http://[fe80::1]/repo.git",
		"http://[fe80::1%25lo0]/repo.git",
		"http://[::]/repo.git",
		"http://0.0.0.0/repo.git",
		"http://0.1.2.3/repo.git",
		"http://2130706433/repo.git",
		"http://0x7f000001/repo.git",
		"http://017700000001/repo.git",
		"http://127.1/repo.git",
		"http://2852039166/repo.git",
		"http://0xA9FEA9FE/repo.git",
		"http://2886729729/repo.git",
		"http://3232235777/repo.git",
	}
	for _, u := range blocked {
		if err := ValidateGitSourceURL(u); err == nil {
			t.Errorf("expected %q to be rejected as private/internal, got nil", u)
		}
	}
}

func TestValidateGitSourceURL_AllowsPublicHosts(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	ok := []string{
		"https://github.com/kubestellar/hive.git",
		"https://gitlab.com/group/project.git",
		"https://8.8.8.8/repo.git", // public IP literal
		"https://[2001:4860:4860::8888]/repo.git",
	}
	for _, u := range ok {
		if err := ValidateGitSourceURL(u); err != nil {
			t.Errorf("expected %q to pass, got %v", u, err)
		}
	}
}

func TestValidateGitSourceURL_OptInAllowsPrivate(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")
	if err := ValidateGitSourceURL("http://10.0.0.5/internal.git"); err != nil {
		t.Errorf("opt-in must allow a private internal Git server, got %v", err)
	}
}
