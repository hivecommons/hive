package hub

import (
	cryptoRand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

const (
	saasHivesDir          = "/data/saas/hives"
	maxHivesPerUser       = 3
	maxSaaSHivesTotal     = 5
	provisionPollInterval = 30 * time.Second
	provisionTimeout      = 5 * time.Minute
	cpuRequest            = "500m"
	cpuLimit              = "2000m"
	memRequest            = "2Gi"
	memLimit              = "8Gi"
	nfsStorageCapacity    = "50Gi"
	nfsMountTargetIP      = "10.0.10.30"
	nfsExportPathPrefix   = "/hive-"
	rolloutMaxSurge       = 1
	rolloutMaxUnavailable = 0
)

// defaultClusterID is the cluster ID assigned to hives that predate
// multi-cluster support.
const defaultClusterID = "hive-oke"

// clustersConfigPath is the on-disk location where the hub reads cluster
// definitions. It is a JSON array of ClusterConfig objects.
const clustersConfigPath = "/data/saas/clusters.json"

// ClusterConfig describes a Kubernetes cluster that the hub can provision
// hive spokes onto. Each cluster has its own kubeconfig, storage backend,
// ingress style, and optional platform-specific settings (OCI, OpenShift).
type ClusterConfig struct {
	ID              string `json:"id" yaml:"id"`
	Name            string `json:"name" yaml:"name"`
	KubeconfigPath  string `json:"kubeconfig_path,omitempty" yaml:"kubeconfig_path,omitempty"`
	Context         string `json:"context" yaml:"context"`
	InCluster       bool   `json:"in_cluster" yaml:"in_cluster"`
	StorageClass    string `json:"storage_class" yaml:"storage_class"`
	StorageType     string `json:"storage_type" yaml:"storage_type"`
	NFSMountIP      string `json:"nfs_mount_ip,omitempty" yaml:"nfs_mount_ip,omitempty"`
	IngressType     string `json:"ingress_type" yaml:"ingress_type"`
	IngressClass    string `json:"ingress_class,omitempty" yaml:"ingress_class,omitempty"`
	CertIssuer      string `json:"cert_issuer,omitempty" yaml:"cert_issuer,omitempty"`
	Domain          string `json:"domain" yaml:"domain"`
	DomainPrefix    string `json:"domain_prefix,omitempty" yaml:"domain_prefix,omitempty"`
	OCICompartment  string `json:"oci_compartment,omitempty" yaml:"oci_compartment,omitempty"`
	OCIAvailDomain  string `json:"oci_avail_domain,omitempty" yaml:"oci_avail_domain,omitempty"`
	OCIMountTarget  string `json:"oci_mount_target,omitempty" yaml:"oci_mount_target,omitempty"`
	OCIExportSet    string `json:"oci_export_set,omitempty" yaml:"oci_export_set,omitempty"`
	RequiresSCC     bool   `json:"requires_scc" yaml:"requires_scc"`
	SCCName         string `json:"scc_name,omitempty" yaml:"scc_name,omitempty"`
	HasGPU          bool   `json:"has_gpu" yaml:"has_gpu"`
	Arch            string `json:"arch" yaml:"arch"`
	ImageTag        string `json:"image_tag" yaml:"image_tag"`
	ImagePullPolicy string `json:"image_pull_policy,omitempty" yaml:"image_pull_policy,omitempty"`
}

// kubectlForCluster builds an exec.Cmd that targets a specific cluster.
// For in-cluster configs (InCluster == true) it runs plain kubectl;
// for remote clusters it injects --kubeconfig and --context flags.
func kubectlForCluster(cluster *ClusterConfig, args ...string) *exec.Cmd {
	fullArgs := []string{}
	if !cluster.InCluster {
		fullArgs = append(fullArgs, "--kubeconfig", cluster.KubeconfigPath, "--context", cluster.Context)
	}
	fullArgs = append(fullArgs, args...)
	return exec.Command("kubectl", fullArgs...)
}

// loadClusters reads the clusters config file and returns a validated
// map of cluster ID → ClusterConfig. If the file does not exist, it
// returns a single default entry for hive-oke (backward compatibility).
func loadClusters(logger *slog.Logger) map[string]ClusterConfig {
	clusters := make(map[string]ClusterConfig)

	data, err := os.ReadFile(clustersConfigPath)
	if err != nil {
		logger.Info("no clusters config found, using default hive-oke cluster", "path", clustersConfigPath)
		clusters[defaultClusterID] = ClusterConfig{
			ID:          defaultClusterID,
			Name:        "OKE (default)",
			InCluster:   true,
			StorageType: "nfs",
			IngressType: "nginx",
			Domain:      "hive.kubestellar.io",
			Arch:        "arm64",
			ImageTag:    "v2-latest",
		}
		return clusters
	}

	var configs []ClusterConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		logger.Error("failed to parse clusters config", "path", clustersConfigPath, "error", err)
		return clusters
	}

	for _, c := range configs {
		if c.ID == "" {
			logger.Warn("skipping cluster config with empty ID")
			continue
		}
		if !c.InCluster && c.KubeconfigPath == "" {
			logger.Warn("skipping remote cluster with no kubeconfig_path", "cluster", c.ID)
			continue
		}
		if c.Domain == "" {
			logger.Warn("skipping cluster with no domain", "cluster", c.ID)
			continue
		}
		clusters[c.ID] = c
	}

	logger.Info("loaded cluster configs", "count", len(clusters))
	return clusters
}

type PendingAccessRequest struct {
	Username    string `json:"username"`
	RequestedAt string `json:"requested_at"`
}

type SaaSHive struct {
	ID              string                 `json:"id"`
	Owner           string                 `json:"owner"`
	ProjectName     string                 `json:"project_name"`
	Org             string                 `json:"org"`
	Repos           []string               `json:"repos"`
	PrimaryRepo     string                 `json:"primary_repo"`
	ACMMLevel       int                    `json:"acmm_level"`
	Status          string                 `json:"status"`
	CreatedAt       string                 `json:"created_at"`
	Subdomain       string                 `json:"subdomain"`
	Error           string                 `json:"error,omitempty"`
	AutoUpgrade     bool                   `json:"auto_upgrade"`
	PendingRequests []PendingAccessRequest `json:"pending_requests,omitempty"`
	OCIFileSystemID string                 `json:"oci_file_system_id,omitempty"`
	OCIExportID     string                 `json:"oci_export_id,omitempty"`
	ClusterID       string                 `json:"cluster_id,omitempty"`
}

type CreateHiveRequest struct {
	Org            string `json:"org"`
	Repos          string `json:"repos"`
	PrimaryRepo    string `json:"primary_repo"`
	ProjectName    string `json:"project_name"`
	ACMMLevel      int    `json:"acmm_level"`
	GitHubToken    string `json:"github_token"`
	AuthMethod     string `json:"auth_method"`
	AppID          string `json:"app_id"`
	InstallationID string `json:"installation_id"`
	AppPrivateKey  string `json:"app_private_key"`
}

func generateHiveID(org, repo string) string {
	short := repo
	if len(short) > 12 {
		short = short[:12]
	}
	chars := "abcdefghijklmnopqrstuvwxyz0123456789"
	suffix := make([]byte, 4)
	for i := range suffix {
		suffix[i] = chars[rand.Intn(len(chars))]
	}
	return fmt.Sprintf("hosted-%s-%s-%s", sanitize(org), sanitize(short), string(suffix))
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// clusterForHive returns the ClusterConfig for a given hive, falling back
// to the default cluster if the hive has no cluster_id or the ID is unknown.
func (s *HubServer) clusterForHive(h *SaaSHive) *ClusterConfig {
	clusterID := h.ClusterID
	if clusterID == "" {
		clusterID = defaultClusterID
	}
	if c, ok := s.clusters[clusterID]; ok {
		return &c
	}
	// Fallback: return the default cluster if it exists.
	if c, ok := s.clusters[defaultClusterID]; ok {
		return &c
	}
	return nil
}

// hubCluster returns the cluster config for the hub itself (always the
// default in-cluster config). Hub operations like self-upgrade always
// target this cluster.
func (s *HubServer) hubCluster() *ClusterConfig {
	if c, ok := s.clusters[defaultClusterID]; ok {
		return &c
	}
	// Fallback: synthesize an in-cluster config so hub operations still work.
	return &ClusterConfig{ID: defaultClusterID, InCluster: true}
}

func loadSaaSHive(id string) *SaaSHive {
	if strings.Contains(id, "..") || strings.Contains(id, "/") || strings.Contains(id, "\\") {
		return nil
	}
	path := filepath.Join(saasHivesDir, id, "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var h SaaSHive
	if json.Unmarshal(data, &h) != nil {
		return nil
	}
	return &h
}

func saveSaaSHive(h *SaaSHive) error {
	if strings.Contains(h.ID, "..") || strings.Contains(h.ID, "/") || strings.Contains(h.ID, "\\") {
		return fmt.Errorf("invalid hive ID for save: %q", h.ID)
	}
	dir := filepath.Join(saasHivesDir, h.ID)
	os.MkdirAll(dir, 0o755)
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "meta.json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func listSaaSHives() []SaaSHive {
	entries, err := os.ReadDir(saasHivesDir)
	if err != nil {
		return nil
	}
	var hives []SaaSHive
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		h := loadSaaSHive(e.Name())
		if h != nil {
			hives = append(hives, *h)
		}
	}
	return hives
}

func countUserHives(username string) int {
	count := 0
	for _, h := range listSaaSHives() {
		if h.Owner == username {
			count++
		}
	}
	return count
}

func provisionHive(h *SaaSHive, req *CreateHiveRequest, cluster *ClusterConfig, logger *slog.Logger) error {
	dir := filepath.Join(saasHivesDir, h.ID, "manifests")
	os.MkdirAll(dir, 0o755)

	repos := h.Repos
	reposYAML := "[]"
	if len(repos) > 0 {
		parts := make([]string, 0, len(repos))
		for _, r := range repos {
			clean := sanitize(r)
			if clean != "" {
				parts = append(parts, fmt.Sprintf("      - %s", clean))
			}
		}
		if len(parts) > 0 {
			reposYAML = "\n" + strings.Join(parts, "\n")
		}
	}

	useApp := req.AuthMethod == "app" && req.AppID != "" && req.InstallationID != "" && req.AppPrivateKey != ""

	data := map[string]any{
		"ID":             h.ID,
		"Namespace":      "hive-hosted-" + h.ID,
		"Org":            sanitize(h.Org),
		"Repos":          reposYAML,
		"PrimaryRepo":    sanitize(h.PrimaryRepo),
		"ACMMLevel":      h.ACMMLevel,
		"Token":          req.GitHubToken,
		"UseApp":         useApp,
		"AppID":          sanitize(req.AppID),
		"InstallationID": sanitize(req.InstallationID),
		"AppPrivateKey": func() string {
			lines := strings.Split(strings.TrimSpace(req.AppPrivateKey), "\n")
			for i := range lines {
				lines[i] = "    " + strings.TrimSpace(lines[i])
			}
			return strings.Join(lines, "\n")
		}(),
		"CPURequest":           cpuRequest,
		"CPULimit":             cpuLimit,
		"MemRequest":           memRequest,
		"MemLimit":             memLimit,
		"NFSStorageCapacity":   nfsStorageCapacity,
		"NFSMountTargetIP":     nfsMountTargetIP,
		"NFSExportPath":        nfsExportPathPrefix + h.ID,
		"PVName":               "hive-" + h.ID + "-fss-pv",
		"RolloutMaxSurge":      rolloutMaxSurge,
		"RolloutMaxUnavailable": rolloutMaxUnavailable,
		"DashboardToken": func() string {
			const tokenBytes = 32
			b := make([]byte, tokenBytes)
			if _, err := cryptoRand.Read(b); err != nil {
				logger.Error("failed to generate dashboard token", "error", err)
				return ""
			}
			return hex.EncodeToString(b)
		}(),
		"HubSecret": func() string {
			if s := os.Getenv("HIVE_HUB_SECRET"); s != "" {
				return s
			}
			if data, err := os.ReadFile("/data/saas/hub-secret.key"); err == nil {
				return strings.TrimSpace(string(data))
			}
			return ""
		}(),
	}

	// Auto-create OCI File System + NFS export for this hive.
	// Failures are non-fatal — the admin can create them manually.
	exportPath := nfsExportPathPrefix + h.ID
	fsID, err := createOCIFileSystem("hive-"+h.ID, logger)
	if err != nil {
		logger.Warn("OCI FSS creation failed — admin must create manually", "hive", h.ID, "error", err)
	} else {
		h.OCIFileSystemID = fsID
		exportID, exportErr := createOCIExport(fsID, exportPath, logger)
		if exportErr != nil {
			logger.Warn("OCI export creation failed — admin must create manually", "hive", h.ID, "error", exportErr)
		} else {
			h.OCIExportID = exportID
		}
	}

	tmpl, err := template.New("manifests").Parse(k8sManifestTemplate)
	if err != nil {
		return fmt.Errorf("template parse: %w", err)
	}

	manifestPath := filepath.Join(dir, "all.yaml")
	f, err := os.Create(manifestPath)
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		f.Close()
		return fmt.Errorf("template exec: %w", err)
	}
	f.Close()

	cmd := kubectlForCluster(cluster, "apply", "-f", manifestPath)
	out, err := cmd.CombinedOutput()

	// Remove manifest immediately — it contains GitHub tokens in plaintext
	if rmErr := os.Remove(manifestPath); rmErr != nil {
		logger.Error("SECURITY: failed to remove manifest with plaintext tokens", "path", manifestPath, "error", rmErr)
	}

	if err != nil {
		logger.Warn("kubectl apply failed", "hive", h.ID, "cluster", cluster.ID, "output", string(out), "error", err)
		return fmt.Errorf("provisioning failed — check hub logs for details")
	}

	logger.Info("audit: saas hive provisioned", "hive_id", h.ID, "owner", h.Owner, "org", h.Org, "cluster", cluster.ID)
	return nil
}

// deprovisionHive performs best-effort cleanup of all resources associated
// with a hosted hive: K8s namespace (cascading to deployment, service, ingress,
// configmap, secret, PVC), cluster-scoped PV, OCI NFS export, OCI file system,
// and the on-disk SaaS hive record. It also decrements the owner's quota.
// Order matters: namespace before PV, export before file system.
// Errors are logged but do not stop the remaining cleanup steps.
func deprovisionHive(h *SaaSHive, cluster *ClusterConfig, logger *slog.Logger) {
	hiveID := h.ID

	// Step 1: Delete K8s namespace (cascading delete removes deployment, service,
	// ingress, configmap, secret, and PVC).
	ns := "hive-hosted-" + hiveID
	logger.Info("deprovision: deleting namespace", "namespace", ns, "cluster", cluster.ID)
	cmd := kubectlForCluster(cluster, "delete", "namespace", ns, "--ignore-not-found")
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("deprovision: namespace delete failed", "namespace", ns, "output", string(out), "error", err)
	}

	// Step 2: Delete cluster-scoped PV (not removed by namespace deletion).
	pvName := "hive-" + hiveID + "-fss-pv"
	logger.Info("deprovision: deleting PV", "pv", pvName)
	cmd = kubectlForCluster(cluster, "delete", "pv", pvName, "--ignore-not-found")
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("deprovision: PV delete failed", "pv", pvName, "output", string(out), "error", err)
	}

	// Step 3: Delete OCI NFS export (must happen before file system deletion).
	if h.OCIExportID != "" {
		logger.Info("deprovision: deleting OCI export", "exportID", h.OCIExportID)
		if err := deleteOCIExport(h.OCIExportID, logger); err != nil {
			logger.Warn("deprovision: OCI export delete failed", "exportID", h.OCIExportID, "error", err)
		}
	}

	// Step 4: Delete OCI file system (must be empty of exports first).
	if h.OCIFileSystemID != "" {
		logger.Info("deprovision: deleting OCI file system", "fileSystemID", h.OCIFileSystemID)
		if err := deleteOCIFileSystem(h.OCIFileSystemID, logger); err != nil {
			logger.Warn("deprovision: OCI file system delete failed", "fileSystemID", h.OCIFileSystemID, "error", err)
		}
	}

	// Step 5: Remove the on-disk SaaS hive record.
	hiveDir := filepath.Join(saasHivesDir, hiveID)
	if err := os.RemoveAll(hiveDir); err != nil {
		logger.Warn("deprovision: failed to remove hive directory", "path", hiveDir, "error", err)
	}

	// Step 6: Remove hive from all user records (decrement quota).
	for _, u := range listAllSaaSUsers() {
		if _, ok := u.Hives[hiveID]; ok {
			delete(u.Hives, hiveID)
			if err := saveSaaSUser(&u); err != nil {
				logger.Warn("deprovision: failed to update user record", "user", u.GitHubUsername, "error", err)
			}
		}
	}

	logger.Info("audit: hive deprovisioned", "hive_id", hiveID, "owner", h.Owner)
}

func (s *HubServer) startProvisionWatcher() {
	ticker := time.NewTicker(provisionPollInterval)
	defer ticker.Stop()

	for range ticker.C {
		hives := listSaaSHives()
		for _, h := range hives {
			if h.Status != "provisioning" {
				continue
			}
			created, _ := time.Parse(time.RFC3339, h.CreatedAt)
			if time.Since(created) > provisionTimeout {
				h.Status = "error"
				h.Error = "provisioning timed out"
				saveSaaSHive(&h)
				s.logger.Warn("saas hive provision timeout", "hive_id", h.ID)
				continue
			}

			cluster := s.clusterForHive(&h)
			if cluster == nil {
				s.logger.Warn("no cluster config for hive", "hive_id", h.ID, "cluster_id", h.ClusterID)
				continue
			}
			ns := "hive-hosted-" + h.ID
			cmd := kubectlForCluster(cluster, "get", "deployment", "hive", "-n", ns, "-o", "jsonpath={.status.availableReplicas}")
			out, err := cmd.Output()
			if err != nil {
				continue
			}
			if strings.TrimSpace(string(out)) == "1" {
				h.Status = "running"
				saveSaaSHive(&h)
				s.logger.Info("audit: saas hive running", "hive_id", h.ID, "cluster", cluster.ID)
			}
		}
	}
}

const k8sManifestTemplate = `apiVersion: v1
kind: Namespace
metadata:
  name: {{.Namespace}}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: hive-config
  namespace: {{.Namespace}}
data:
  hive.yaml: |
    project:
      org: {{.Org}}
      repos: {{.Repos}}
      primary_repo: {{.PrimaryRepo}}
    agents:
      guide:
        backend: copilot
        model: claude-sonnet-4-6
        enabled: true
      scanner:
        backend: copilot
        model: claude-sonnet-4-6
        enabled: true
    governor:
      eval_interval_s: 300
      modes:
        idle:
          threshold: 0
          guide: 4h
          scanner: 4h
        busy:
          threshold: 10
          guide: 2h
          scanner: 2h
    github:
{{- if .UseApp}}
      app_id: {{.AppID}}
      installation_id: {{.InstallationID}}
      key_file: /secrets/gh-app-key.pem
{{- else}}
      token: "${HIVE_GITHUB_TOKEN}"
{{- end}}
    dashboard:
      port: 3002
    hub:
      enabled: true
      url: https://hive.kubestellar.io
      is_public: true
    acmm_level: {{.ACMMLevel}}
---
apiVersion: v1
kind: Secret
metadata:
  name: hive-secrets
  namespace: {{.Namespace}}
type: Opaque
stringData:
  dashboard-token: {{.DashboardToken}}
{{- if .UseApp}}
  gh-app-key.pem: |
{{.AppPrivateKey}}
{{- else}}
  github-token: {{.Token}}
{{- end}}
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: {{.PVName}}
spec:
  capacity:
    storage: {{.NFSStorageCapacity}}
  accessModes:
    - ReadWriteMany
  persistentVolumeReclaimPolicy: Retain
  nfs:
    server: {{.NFSMountTargetIP}}
    path: {{.NFSExportPath}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: hive-data
  namespace: {{.Namespace}}
spec:
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: {{.NFSStorageCapacity}}
  volumeName: {{.PVName}}
  storageClassName: ""
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hive
  namespace: {{.Namespace}}
spec:
  replicas: 1
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: {{.RolloutMaxSurge}}
      maxUnavailable: {{.RolloutMaxUnavailable}}
  selector:
    matchLabels:
      app: hive
      hive-id: {{.ID}}
  template:
    metadata:
      labels:
        app: hive
        hive-id: {{.ID}}
    spec:
      initContainers:
      - name: copy-config
        image: ghcr.io/kubestellar/hive:v2-latest
        imagePullPolicy: Always
        command: ["sh", "-c", "cp /etc/hive-seed/hive.yaml /etc/hive/hive.yaml && echo configmap-copied; if [ -f /data/hive.yaml.bak ]; then echo backup-exists-for-recovery; fi"]
        volumeMounts:
        - name: config
          mountPath: /etc/hive-seed
          readOnly: true
        - name: config-writable
          mountPath: /etc/hive
        - name: data
          mountPath: /data
      containers:
      - name: hive
        image: ghcr.io/kubestellar/hive:v2-latest
        imagePullPolicy: Always
        securityContext:
          capabilities:
            add:
            - NET_ADMIN
        readinessProbe:
          httpGet:
            path: /api/health
            port: 3001
          initialDelaySeconds: 5
          periodSeconds: 5
          failureThreshold: 3
        startupProbe:
          httpGet:
            path: /api/health
            port: 3001
          initialDelaySeconds: 10
          periodSeconds: 5
          failureThreshold: 30
        livenessProbe:
          httpGet:
            path: /api/health
            port: 3001
          periodSeconds: 30
          failureThreshold: 3
        env:
{{- if not .UseApp}}
        - name: HIVE_GITHUB_TOKEN
          valueFrom:
            secretKeyRef:
              name: hive-secrets
              key: github-token
{{- end}}
        - name: DASHBOARD_AUTH_TOKEN
          valueFrom:
            secretKeyRef:
              name: hive-secrets
              key: dashboard-token
        - name: HIVE_ID
          value: "{{.ID}}"
        - name: HIVE_LEVEL
          value: "{{.ACMMLevel}}"
        - name: HIVE_HUB_URL
          value: https://hive.kubestellar.io
        - name: HIVE_HUB_SECRET
          value: "{{.HubSecret}}"
        ports:
        - containerPort: 3001
        resources:
          requests:
            cpu: {{.CPURequest}}
            memory: {{.MemRequest}}
          limits:
            cpu: {{.CPULimit}}
            memory: {{.MemLimit}}
        volumeMounts:
        - name: config-writable
          mountPath: /etc/hive
        - name: data
          mountPath: /data
{{- if .UseApp}}
        - name: secrets
          mountPath: /secrets
          readOnly: true
{{- end}}
      volumes:
      - name: config
        configMap:
          name: hive-config
      - name: config-writable
        emptyDir: {}
      - name: data
        persistentVolumeClaim:
          claimName: hive-data
{{- if .UseApp}}
      - name: secrets
        secret:
          secretName: hive-secrets
{{- end}}
---
apiVersion: v1
kind: Service
metadata:
  name: hive
  namespace: {{.Namespace}}
spec:
  selector:
    app: hive
    hive-id: {{.ID}}
  ports:
  - name: http
    port: 3001
    targetPort: 3001
  type: ClusterIP
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: hive
  namespace: {{.Namespace}}
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/auth-url: "https://hive.kubestellar.io/api/saas/auth-check?hive={{.ID}}&uri=$request_uri"
    nginx.ingress.kubernetes.io/custom-http-errors: "502,503"
    nginx.ingress.kubernetes.io/default-backend: hive-error-pages
    nginx.ingress.kubernetes.io/auth-signin: "https://hive.kubestellar.io/login?redirect=$scheme://$http_host$request_uri"
    nginx.ingress.kubernetes.io/auth-response-headers: "X-Hive-User,X-Hive-Role"
spec:
  ingressClassName: nginx
  rules:
  - host: {{.ID}}.hive.kubestellar.io
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: hive
            port:
              number: 3001
  tls:
  - hosts:
    - {{.ID}}.hive.kubestellar.io
    secretName: hive-tls
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: hive-contribute
  namespace: {{.Namespace}}
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
spec:
  ingressClassName: nginx
  rules:
  - host: {{.ID}}.hive.kubestellar.io
    http:
      paths:
      - path: /api/contribute
        pathType: Prefix
        backend:
          service:
            name: hive
            port:
              number: 3001
  tls:
  - hosts:
    - {{.ID}}.hive.kubestellar.io
    secretName: hive-tls
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: hive-terminal
  namespace: {{.Namespace}}
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
spec:
  ingressClassName: nginx
  rules:
  - host: {{.ID}}.hive.kubestellar.io
    http:
      paths:
      - path: /terminal
        pathType: Prefix
        backend:
          service:
            name: hive
            port:
              number: 3001
  tls:
  - hosts:
    - {{.ID}}.hive.kubestellar.io
    secretName: hive-tls
`
