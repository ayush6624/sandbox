package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/agentapi"
	"github.com/ayush6624/sandbox/internal/client"
)

// `sandbox template build` turns a container image into a sandbox template: the
// image's filesystem becomes the guest's rootfs, not a container running inside
// one. The pipeline is
//
//	docker export → extract → overlay the sandbox contract → mkfs.ext4 → boot once → snapshot
//
// and the snapshot id it prints IS the template id — create from it with
// `source: {type: "snapshot", id}`. Snapshots are already fleet-portable (a
// worker without the artifacts pulls them from the snapshot bucket), so nothing
// else has to distribute a template.
//
// The one thing the image must provide, because the guest is a machine rather
// than a container, is /bin/bash — exec and the pty shell are `bash -l`. This
// fails fast and says so rather than producing a template that boots and then
// misbehaves. Notably it does NOT need iproute2: the agent reconfigures eth0
// over netlink when `ip` is absent (cmd/sandboxd/netlink_linux.go), so a
// published image works unmodified.
//
// The image's ENTRYPOINT/CMD and any init system are NOT run: sandboxd is PID 1
// (cmd/sandboxd/init_linux.go). The image's ENV is preserved via /etc/profile.d.

// templateEnvFile carries the image's ENV into the login shells that /exec and
// /shell run. zz- so it wins over other profile.d entries and over the PATH
// /etc/profile itself sets.
const templateEnvFile = "etc/profile.d/zz-sandbox-template.sh"

// envPassthroughSkip names variables the guest owns rather than the image:
// sandboxd sets HOME/USER/LOGNAME per exec, and the rest are per-session.
var envPassthroughSkip = map[string]bool{
	"HOME": true, "USER": true, "LOGNAME": true, "PWD": true,
	"SHLVL": true, "HOSTNAME": true, "TERM": true, "_": true,
}

func templateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "Build sandbox templates from container images",
	}
	cmd.AddCommand(templateBuildCmd())
	cmd.AddCommand(templateListCmd(), templateWarmCmd())
	return cmd
}

func templateListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "list", Short: "List reusable templates and their warm targets",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			snaps, err := c.ListSnapshots(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Println("ID\tNAME\tWARM")
			for _, snap := range snaps {
				if snap.Role == "template" {
					fmt.Printf("%s\t%s\t%d\n", snap.ID, snap.Name, snap.WarmTarget)
				}
			}
			return nil
		},
	}
	addClientFlags(cmd)
	return cmd
}

func templateWarmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "warm TEMPLATE_ID COUNT", Short: "Set a template's per-worker ready target", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := strconv.Atoi(args[1])
			if err != nil || target < 0 {
				return fmt.Errorf("COUNT must be a non-negative integer")
			}
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			snap, err := c.SetTemplateWarmTarget(cmd.Context(), args[0], target)
			if err != nil {
				return err
			}
			fmt.Printf("%s warm target: %d\n", snap.ID, snap.WarmTarget)
			return nil
		},
	}
	addClientFlags(cmd)
	return cmd
}

func templateBuildCmd() *cobra.Command {
	var (
		fromImage string
		fromTar   string
		name      string
		size      string
		agentBin  string
		vcpus     int64
		memMIB    int64
	)
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Turn a container image into a template snapshot (root required)",
		Example: "  sandbox template build --from-image python:3.13-slim --name py313\n" +
			"  docker build -t my-env . && sandbox template build --from-image my-env --name my-env",
		RunE: func(cmd *cobra.Command, args []string) error {
			if (fromImage == "") == (fromTar == "") {
				return fmt.Errorf("pass exactly one of --from-image or --from-tar")
			}
			if os.Geteuid() != 0 {
				return fmt.Errorf("must run as root (extracts an image and builds a filesystem)")
			}
			return templateBuild(cmd.Context(), templateBuildArgs{
				fromImage: fromImage, fromTar: fromTar, name: name,
				size: size, agentBin: agentBin, vcpus: vcpus, memMIB: memMIB,
			})
		},
	}
	addClientFlags(cmd)
	cmd.Flags().StringVar(&fromImage, "from-image", "", "container image reference to build from (needs docker on this host)")
	cmd.Flags().StringVar(&fromTar, "from-tar", "", "flattened filesystem tar to build from (e.g. `docker export` output)")
	cmd.Flags().StringVar(&name, "name", "", "template name (recorded on the snapshot)")
	cmd.Flags().StringVar(&size, "size", "10G", "rootfs size (sparse; must fit the image plus the workload's writes)")
	cmd.Flags().StringVar(&agentBin, "agent", "./sandboxd", "path to the sandboxd binary to install")
	cmd.Flags().Int64Var(&vcpus, "vcpus", 0, "vCPUs baked into the template (0 = host default)")
	cmd.Flags().Int64Var(&memMIB, "mem-mib", 0, "guest memory baked into the template (0 = host default)")
	return cmd
}

type templateBuildArgs struct {
	fromImage, fromTar, name, size, agentBin string
	vcpus, memMIB                            int64
}

func templateBuild(ctx context.Context, args templateBuildArgs) error {
	cfg, c, err := dialClient()
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("template build talks to a worker directly; run it on the host, without --api-url")
	}

	// Work beside the per-sandbox rootfs copies: same filesystem (so the
	// mkfs output can be reflinked into the build VM's disk) and same disk
	// budget, which /tmp on a fleet worker does not have.
	workDir := filepath.Join(filepath.Dir(cfg.RootfsDir), "templates")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	stage, err := os.MkdirTemp(workDir, "build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	tarPath := args.fromTar
	var imgCfg imageConfig
	if args.fromImage != "" {
		logf("exporting %s", args.fromImage)
		if tarPath, err = dockerExport(args.fromImage, stage); err != nil {
			return err
		}
		if imgCfg, err = dockerImageConfig(args.fromImage); err != nil {
			return err
		}
	}

	root := filepath.Join(stage, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	logf("extracting filesystem")
	if out, err := exec.Command("tar", "-xpf", tarPath, "-C", root, "--numeric-owner").CombinedOutput(); err != nil {
		return fmt.Errorf("extract %s: %w: %s", tarPath, err, out)
	}

	logf("overlaying the sandbox agent")
	if err := overlayTemplateRootfs(root, args.agentBin, imgCfg); err != nil {
		return err
	}

	image := filepath.Join(stage, "rootfs.ext4")
	logf("building ext4 (%s)", args.size)
	if err := makeExt4(image, root, args.size); err != nil {
		return err
	}

	logf("booting it once and snapshotting")
	snap, err := c.BuildTemplate(ctx, client.TemplateBuildOpts{
		RootfsPath: image, Name: args.name, Vcpus: args.vcpus, MemMIB: args.memMIB,
	})
	if err != nil {
		return fmt.Errorf("build template: %w", err)
	}
	logf("template ready: %s", snap.ID)
	logf("create from it with source {\"type\":\"snapshot\",\"id\":\"%s\"}", snap.ID)
	fmt.Println(snap.ID)
	return nil
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, ">> "+format+"\n", a...)
}

// dockerExport flattens an image into a tar. `docker export` needs a container,
// not an image, so this creates one without running it.
func dockerExport(ref, stage string) (string, error) {
	out, err := exec.Command("docker", "create", ref).Output()
	if err != nil {
		return "", fmt.Errorf("docker create %s: %w", ref, err)
	}
	cid := strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", "-f", cid).Run()

	tarPath := filepath.Join(stage, "image.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	cmd := exec.Command("docker", "export", cid)
	cmd.Stdout, cmd.Stderr = f, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker export %s: %w", cid, err)
	}
	return tarPath, nil
}

// imageConfig is the part of a container image's config that describes how its
// processes are meant to run. All three are contract, not decoration: a
// workload built for the image expects its ENV, its USER, and its WORKDIR.
type imageConfig struct {
	Env        []string `json:"Env"`
	User       string   `json:"User"`
	WorkingDir string   `json:"WorkingDir"`
}

func dockerImageConfig(ref string) (imageConfig, error) {
	var cfg imageConfig
	out, err := exec.Command("docker", "image", "inspect", "-f", "{{json .Config}}", ref).Output()
	if err != nil {
		return cfg, fmt.Errorf("docker image inspect %s: %w", ref, err)
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return cfg, fmt.Errorf("decode image config: %w", err)
	}
	return cfg, nil
}

// guestProfileFor maps an image config onto the identity sandboxd should adopt.
// An image with no USER runs as root — that is Docker's default and what
// workloads written for it assume — and its WORKDIR becomes the exec cwd.
func guestProfileFor(cfg imageConfig) agentapi.GuestProfile {
	profile := agentapi.GuestProfile{User: cfg.User, Cwd: cfg.WorkingDir}
	// "user:group" — the group comes from the account itself.
	if name, _, ok := strings.Cut(profile.User, ":"); ok {
		profile.User = name
	}
	// A USER may be given as a uid; sandboxd resolves by name, so only a name
	// is usable. Fall back to root rather than guessing wrong.
	if _, err := strconv.Atoi(profile.User); err == nil {
		profile.User = ""
	}
	if profile.User == "" {
		profile.User = "root"
	}
	if profile.Cwd == "" {
		profile.Cwd = "/"
	}
	return profile
}

// overlayTemplateRootfs installs everything the guest side of the sandbox
// contract needs into an extracted image: the agent, the account exec runs as,
// and the drop-ins that make sudo/sshd behave as they do in the base image.
func overlayTemplateRootfs(root, agentBin string, cfg imageConfig) error {
	if err := requireGuestBinaries(root); err != nil {
		return err
	}

	bin, err := os.ReadFile(agentBin)
	if err != nil {
		return fmt.Errorf("agent binary: %w", err)
	}
	agentDest := filepath.Join(root, strings.TrimPrefix(agentapi.AgentPath, "/"))
	if err := os.MkdirAll(filepath.Dir(agentDest), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(agentDest, bin, 0o755); err != nil {
		return fmt.Errorf("install agent: %w", err)
	}

	uid, gid, err := ensureGuestAccount(root)
	if err != nil {
		return err
	}
	home := filepath.Join(root, "home/sandbox")
	if err := os.MkdirAll(filepath.Join(home, "app"), 0o755); err != nil {
		return err
	}
	for _, f := range []struct{ name, body string }{
		{".profile", sandboxProfile}, {".bashrc", sandboxBashrc},
	} {
		if err := os.WriteFile(filepath.Join(home, f.name), []byte(f.body), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	if err := chownTree(home, uid, gid); err != nil {
		return err
	}

	// sudo and sshd are optional in a container image; configure each only if
	// the image actually ships it, and let the corresponding feature be absent
	// otherwise rather than failing the build.
	if fileExists(filepath.Join(root, "usr/bin/sudo")) {
		if err := writeFileIn(root, "etc/sudoers.d/sandbox", sandboxSudoers, 0o440); err != nil {
			return err
		}
	}
	if dirExists(filepath.Join(root, "etc/ssh")) {
		if err := writeFileIn(root, "etc/ssh/sshd_config.d/sandbox.conf", sandboxSSHDConfig, 0o644); err != nil {
			return err
		}
		// Never ship an image-baked host key: /identity rotates one per
		// sandbox, and an inherited key is a shared identity until it does.
		keys, _ := filepath.Glob(filepath.Join(root, "etc/ssh/ssh_host_*"))
		for _, key := range keys {
			if err := os.Remove(key); err != nil {
				return fmt.Errorf("remove baked host key: %w", err)
			}
		}
	}

	if err := writeFileIn(root, "etc/hostname", "sandbox\n", 0o644); err != nil {
		return err
	}
	if err := writeFileIn(root, "etc/hosts", "127.0.0.1\tlocalhost sandbox\n::1\tlocalhost ip6-localhost ip6-loopback\n", 0o644); err != nil {
		return err
	}
	if err := writeFileIn(root, templateEnvFile, imageEnvScript(cfg.Env), 0o644); err != nil {
		return err
	}

	// Record the identity the image declares, so the agent runs work as that
	// user in that directory instead of as the sandbox account.
	guest := guestProfileFor(cfg)
	profile, err := json.Marshal(guest)
	if err != nil {
		return err
	}
	logf("image identity: user=%s cwd=%s", guest.User, guest.Cwd)
	return writeFileIn(root, strings.TrimPrefix(agentapi.GuestProfilePath, "/"), string(profile)+"\n", 0o644)
}

// requireGuestBinaries fails the build on image contents a sandbox cannot work
// without, with the fix rather than just the symptom.
func requireGuestBinaries(root string) error {
	for _, need := range []struct {
		what, fix string
		paths     []string
	}{
		{"bash", "install bash in your image (exec and the pty shell run `bash -l`)",
			[]string{"bin/bash", "usr/bin/bash", "usr/local/bin/bash"}},
	} {
		found := false
		for _, p := range need.paths {
			if fileExists(filepath.Join(root, p)) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("image has no %s: %s", need.what, need.fix)
		}
	}
	return nil
}

// ensureGuestAccount makes `sandbox` resolvable in the image's own /etc/passwd,
// because that is how the agent finds the uid it drops every exec, file
// operation, and shell to (cmd/sandboxd/guestuser.go looks it up BY NAME). It
// keeps an existing sandbox account, and picks a free uid when the image
// already uses 1000 for its own user — the name is what has to match, not the
// number.
func ensureGuestAccount(root string) (int, int, error) {
	passwdPath := filepath.Join(root, "etc/passwd")
	passwd, err := os.ReadFile(passwdPath)
	if err != nil {
		return 0, 0, fmt.Errorf("image has no /etc/passwd: %w", err)
	}
	used := map[int]bool{}
	for _, line := range strings.Split(string(passwd), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		if fields[0] == agentapi.GuestUser {
			gid, err := strconv.Atoi(fields[3])
			if err != nil {
				return 0, 0, fmt.Errorf("account %q has an unparsable gid %q", agentapi.GuestUser, fields[3])
			}
			return uid, gid, nil
		}
		used[uid] = true
	}
	id := 1000
	for used[id] {
		id++
	}
	if err := appendLine(passwdPath, fmt.Sprintf("%s:x:%d:%d:Sandbox:/home/sandbox:/bin/bash", agentapi.GuestUser, id, id)); err != nil {
		return 0, 0, err
	}
	if err := appendLine(filepath.Join(root, "etc/group"), fmt.Sprintf("%s:x:%d:", agentapi.GuestUser, id)); err != nil {
		return 0, 0, err
	}
	// A locked password ("!"), not an absent shadow entry: PAM denies an
	// account it cannot find, which would break sshd's public-key login.
	shadow := filepath.Join(root, "etc/shadow")
	if fileExists(shadow) {
		if err := appendLine(shadow, agentapi.GuestUser+":!:20000:0:99999:7:::"); err != nil {
			return 0, 0, err
		}
	}
	return id, id, nil
}

// imageEnvScript re-exports the image's ENV. Without it a template's PATH is
// whatever /etc/profile sets, and the tools the Dockerfile installed under a
// custom prefix (the usual reason for ENV PATH) are missing from every exec.
func imageEnvScript(env []string) string {
	var b strings.Builder
	b.WriteString("# Managed by `sandbox template build` — the image's ENV.\n")
	for _, item := range env {
		name, value, ok := strings.Cut(item, "=")
		if !ok || name == "" || envPassthroughSkip[name] {
			continue
		}
		fmt.Fprintf(&b, "export %s='%s'\n", name, strings.ReplaceAll(value, "'", `'\''`))
	}
	return b.String()
}

// makeExt4 builds the filesystem from a directory tree, which avoids
// loop-mounting anything: mkfs.ext4 -d copies the tree in as it formats.
func makeExt4(image, root, size string) error {
	if out, err := exec.Command("truncate", "-s", size, image).CombinedOutput(); err != nil {
		return fmt.Errorf("truncate %s: %w: %s", image, err, out)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-d", root, "-F", image).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4: %w: %s", err, out)
	}
	return nil
}

func appendLine(path, line string) error {
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(body) > 0 && body[len(body)-1] != '\n' {
		body = append(body, '\n')
	}
	body = append(body, (line + "\n")...)
	return os.WriteFile(path, body, 0o644)
}

func writeFileIn(root, rel, body string, mode os.FileMode) error {
	dest := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dest, []byte(body), mode); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	// WriteFile does not apply the mode to an existing file, and sudo fails
	// closed on a group-writable sudoers drop-in.
	return os.Chmod(dest, mode)
}

func chownTree(dir string, uid, gid int) error {
	return filepath.Walk(dir, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
