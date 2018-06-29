package child

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/google/uuid"
	"github.com/jamescun/tuntap"
	"github.com/moby/vpnkit/go/pkg/vpnkit"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/AkihiroSuda/rootlesskit/pkg/common"
)

func waitForParentSync(pipeFDStr string) (*common.Message, error) {
	pipeFD, err := strconv.Atoi(pipeFDStr)
	if err != nil {
		return nil, errors.Wrapf(err, "unexpected fd value: %s", pipeFDStr)
	}
	pipeR := os.NewFile(uintptr(pipeFD), "")
	hdr := make([]byte, 4)
	n, err := pipeR.Read(hdr)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read fd %d", pipeFD)
	}
	if n != 4 {
		return nil, errors.Errorf("read %d bytes, expected 4 bytes", n)
	}
	bLen := binary.LittleEndian.Uint32(hdr)
	if bLen > 1<<16 || bLen < 1 {
		return nil, errors.Errorf("bad message size: %d", bLen)
	}
	b := make([]byte, bLen)
	n, err = pipeR.Read(b)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read fd %d", pipeFD)
	}
	if n != int(bLen) {
		return nil, errors.Errorf("read %d bytes, expected %d bytes", n, bLen)
	}
	var msg common.Message
	if err := json.Unmarshal(b, &msg); err != nil {
		return nil, errors.Wrapf(err, "parsing message from fd %d: %q (length %d)", pipeFD, string(b), bLen)
	}
	if err := pipeR.Close(); err != nil {
		return nil, errors.Wrapf(err, "failed to close fd %d", pipeFD)
	}
	return &msg, nil
}

func createCmd(targetCmd []string) (*exec.Cmd, error) {
	var args []string
	if len(targetCmd) > 1 {
		args = targetCmd[1:]
	}
	cmd := exec.Command(targetCmd[0], args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
	}
	return cmd, nil
}

func activateTap(tap, ip string, netmask int, gateway string) error {
	// TODO: use netlink
	cmds := [][]string{
		{"ip", "link", "set", tap, "up"},
		{"ip", "addr", "add", ip + "/" + strconv.Itoa(netmask), "dev", tap},
		{"ip", "route", "add", "default", "via", gateway, "dev", tap},
	}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		return errors.Wrapf(err, "executing %v", cmds)
	}
	return nil
}

func startVPNKitRoutines(ctx context.Context, macStr, socket, uuidStr string) (string, error) {
	tapName := "tap0"
	cmds := [][]string{
		{"ip", "tuntap", "add", "name", tapName, "mode", "tap"},
		{"ip", "link", "set", tapName, "address", macStr},
		{"ip", "link", "set", tapName, "up"},
	}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		return "", errors.Wrapf(err, "executing %v", cmds)
	}
	tap, err := tuntap.Tap(tapName)
	if err != nil {
		return "", errors.Wrapf(err, "creating tap %s", tapName)
	}
	logrus.Debugf("tap=%s", tap.Name())
	vmnet, err := vpnkit.NewVmnet(ctx, socket)
	if err != nil {
		return "", err
	}
	vifUUID, err := uuid.Parse(uuidStr)
	if err != nil {
		return "", err
	}
	vif, err := vmnet.ConnectVif(vifUUID)
	if err != nil {
		return "", err
	}
	go tap2vif(vif, tap)
	go vif2tap(tap, vif)
	return tapName, nil
}

func tap2vif(vif *vpnkit.Vif, r io.Reader) {
	b := make([]byte, 1500)
	for {
		n, err := r.Read(b)
		if err != nil {
			panic(errors.Wrap(err, "tap2vif: read"))
		}
		if err := vif.Write(b[:n]); err != nil {
			panic(errors.Wrap(err, "tap2vif: write"))
		}
	}
}

func vif2tap(w io.Writer, vif *vpnkit.Vif) {
	for {
		b, err := vif.Read()
		if err != nil {
			panic(errors.Wrap(err, "vif2tap: read"))
		}
		if _, err := w.Write(b); err != nil {
			panic(errors.Wrap(err, "vif2tap: write"))
		}
	}
}

func setupNet(msg *common.Message, tempDir string) error {
	if msg.NetworkMode == common.HostNetwork {
		return nil
	}
	tap := ""
	switch msg.NetworkMode {
	case common.VDEPlugSlirp:
		tap = msg.VDEPlugTap
	case common.VPNKit:
		var err error
		tap, err = startVPNKitRoutines(context.TODO(),
			msg.VPNKitMAC,
			msg.VPNKitSocket,
			msg.VPNKitUUID)
		if err != nil {
			return err
		}
	default:
		return errors.Errorf("invalid network mode: %+v", msg.NetworkMode)
	}
	if tap == "" {
		return errors.New("empty tap")
	}
	if err := activateTap(tap, msg.IP, msg.Netmask, msg.Gateway); err != nil {
		return err
	}
	if err := mountResolvConf(tempDir, msg.DNS); err != nil {
		return err
	}
	return nil
}

func Child(pipeFDEnvKey string, targetCmd []string) error {
	pipeFDStr := os.Getenv(pipeFDEnvKey)
	if pipeFDStr == "" {
		return errors.Errorf("%s is not set", pipeFDEnvKey)
	}
	os.Unsetenv(pipeFDEnvKey)
	tempDir, err := ioutil.TempDir("", "rootlesskit-child")
	if err != nil {
		return errors.Wrap(err, "creating temp dir")
	}
	defer os.RemoveAll(tempDir)
	msg, err := waitForParentSync(pipeFDStr)
	if err != nil {
		return err
	}
	logrus.Debugf("child: got msg from parent: %+v", msg)
	if err := setupNet(msg, tempDir); err != nil {
		return err
	}
	cmd, err := createCmd(targetCmd)
	if err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		return errors.Wrapf(err, "command %v exited", targetCmd)
	}
	return nil
}