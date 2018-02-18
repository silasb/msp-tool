package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	dfuDevicePrefix     = "Found DFU: "
	internalFlashMarker = "@Internal Flash  /"
)

// FC represents a connection to the flight controller, which can
// handle disconnections and reconnections on its on. Use NewFC()
// to initialize an FC and then call FC.StartUpdating().
type FC struct {
	opts         FCOptions
	msp          *MSP
	variant      string
	versionMajor byte
	versionMinor byte
	versionPatch byte
	boardID      string
	targetName   string
	features     uint32
}

type FCOptions struct {
	PortName         string
	BaudRate         int
	Stdout           io.Writer
	EnableDebugTrace bool
}

func (f *FCOptions) stderr() io.Writer {
	return f.Stdout
}

// NewFC returns a new FC using the given port and baud rate. stdout is
// optional and will default to os.Stdout if nil
func NewFC(opts FCOptions) (*FC, error) {
	msp, err := NewMSP(opts.PortName, opts.BaudRate)
	if err != nil {
		return nil, err
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	fc := &FC{
		opts: opts,
		msp:  msp,
	}
	fc.updateInfo()
	return fc, nil
}

func (f *FC) reconnect() error {
	if f.msp != nil {
		f.msp.Close()
		f.msp = nil
	}
	for {
		msp, err := NewMSP(f.opts.PortName, f.opts.BaudRate)
		if err == nil {
			f.printf("Reconnected to %s @ %dbps\n", f.opts.PortName, f.opts.BaudRate)
			f.reset()
			f.msp = msp
			f.updateInfo()
			return nil
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *FC) updateInfo() {
	// Send commands to print FC info
	f.msp.WriteCmd(mspAPIVersion)
	f.msp.WriteCmd(mspFCVariant)
	f.msp.WriteCmd(mspFCVersion)
	f.msp.WriteCmd(mspBoardInfo)
	f.msp.WriteCmd(mspBuildInfo)
	f.msp.WriteCmd(mspFeature)
	f.msp.WriteCmd(mspCFSerialConfig)
}

func (f *FC) printf(format string, a ...interface{}) (int, error) {
	return fmt.Fprintf(f.opts.Stdout, format, a...)
}

func (f *FC) printInfo() {
	if f.variant != "" && f.versionMajor != 0 && f.boardID != "" {
		targetName := ""
		if f.targetName != "" {
			targetName = ", target " + f.targetName
		}
		f.printf("%s %d.%d.%d (board %s%s)\n", f.variant, f.versionMajor, f.versionMinor, f.versionPatch, f.boardID, targetName)
	}
}

func (f *FC) handleFrame(fr *MSPFrame) {
	switch fr.Code {
	case mspAPIVersion:
		f.printf("MSP API version %d.%d (protocol %d)\n", fr.Byte(1), fr.Byte(2), fr.Byte(0))
	case mspFCVariant:
		f.variant = string(fr.Payload)
		f.printInfo()
	case mspFCVersion:
		f.versionMajor = fr.Byte(0)
		f.versionMinor = fr.Byte(1)
		f.versionPatch = fr.Byte(2)
		f.printInfo()
	case mspBoardInfo:
		// BoardID is always 4 characters
		f.boardID = string(fr.Payload[:4])
		// Then 4 bytes follow, HW revision (uint16), builtin OSD type (uint8) and wether
		// the board uses VCP (uint8), We ignore those bytes here. Finally, in recent BF
		// and iNAV versions, the length of the targetName (uint8) followed by the target
		// name itself is sent. Try to retrieve it.
		if len(fr.Payload) >= 9 {
			targetNameLength := int(fr.Payload[8])
			if len(fr.Payload) > 8+targetNameLength {
				f.targetName = string(fr.Payload[9 : 9+targetNameLength])
			}
		}
		f.printInfo()
	case mspBuildInfo:
		buildDate := string(fr.Payload[:11])
		buildTime := string(fr.Payload[11:19])
		// XXX: Revision is 8 characters in iNav but 7 in BF/CF
		rev := string(fr.Payload[19:])
		f.printf("Build %s (built on %s @ %s)\n", rev, buildDate, buildTime)
	case mspFeature:
		fr.Read(&f.features)
		if (f.features&mspFCFeatureDebugTrace == 0) && f.shouldEnableDebugTrace() {
			f.printf("Enabling FEATURE_DEBUG_TRACE\n")
			f.features |= mspFCFeatureDebugTrace
			f.msp.WriteCmd(mspSetFeature, f.features)
			f.msp.WriteCmd(mspEepromWrite)
		}
	case mspCFSerialConfig:
		if f.shouldEnableDebugTrace() {
			var cfg MSPSerialConfig
			var serialConfigs []MSPSerialConfig
			hasDebugTraceMSPPort := false
			mask := uint16(serialFunctionMSP | serialFunctionDebugTrace)
			for {
				err := fr.Read(&cfg)
				if err != nil {
					if err == io.EOF {
						// All ports read
						break
					}
					panic(err)
				}
				if cfg.FunctionMask&mask == mask {
					hasDebugTraceMSPPort = true
				}
				serialConfigs = append(serialConfigs, cfg)
			}
			if !hasDebugTraceMSPPort {
				// Enable DEBUG_TRACE on the first MSP port, since DEBUG_TRACE only
				// works on one port.
				for ii := range serialConfigs {
					if serialConfigs[ii].FunctionMask&serialFunctionMSP != 0 {
						f.printf("Enabling FUNCTION_DEBUG_TRACE on serial port %v\n", serialConfigs[ii].Identifier)
						serialConfigs[ii].FunctionMask |= serialFunctionDebugTrace
						break
					}
				}
				// Save ports
				f.msp.WriteCmd(mspSetCFSerialConfig, serialConfigs)
				f.msp.WriteCmd(mspEepromWrite)
			}
		}
	case mspReboot:
		f.printf("Rebooting board...\n")
	case mspDebugMsg:
		s := strings.Trim(string(fr.Payload), " \r\n\t\x00")
		f.printf("[DEBUG] %s\n", s)
	case mspSetFeature:
	case mspSetCFSerialConfig:
	case mspEepromWrite:
		// Nothing to do for these
	default:
		f.printf("Unhandled MSP frame %d with payload %v\n", fr.Code, fr.Payload)
	}
}

func (f *FC) versionGte(major, minor, patch byte) bool {
	return f.versionMajor > major || (f.versionMajor == major && f.versionMinor > minor) ||
		(f.versionMajor == major && f.versionMinor == minor && f.versionPatch >= patch)
}

func (f *FC) shouldEnableDebugTrace() bool {
	// Only INAV 1.9+ supports DEBUG_TRACE for now
	return f.opts.EnableDebugTrace && f.variant == "INAV" && f.versionGte(1, 9, 0)
}

// Reboot reboots the board via MSP_REBOOT
func (f *FC) Reboot() {
	f.msp.WriteCmd(mspReboot)
}

// StartUpdating starts reading from the MSP port and handling
// the received messages. Note that it never returns.
func (f *FC) StartUpdating() {
	for {
		frame, err := f.msp.ReadFrame()
		if err != nil {
			if err == io.EOF {
				f.printf("Board disconnected, trying to reconnect...\n")
				if err := f.reconnect(); err != nil {
					panic(err)
				}
				continue
			}
			if merr, ok := err.(MSPError); ok && merr.IsMSPError() {
				f.printf("%v\n", err)
				continue
			}
			panic(err)
		}
		f.handleFrame(frame)
	}
}

// HasDetectedTargetName returns true iff the target name installed on
// the board has been retrieved via MSP.
func (f *FC) HasDetectedTargetName() bool {
	return f.targetName != ""
}

// Flash compiles the given target and flashes the board
func (f *FC) Flash(srcDir string, targetName string) error {
	if targetName == "" {
		targetName = f.targetName

		if targetName == "" {
			return errors.New("empty target name")
		}
	}
	// First, check that dfu-util is available
	dfu, err := exec.LookPath("dfu-util")
	if err != nil {
		return err
	}
	// Now compile the target
	cmd := exec.Command("make", "binary")
	cmd.Stdout = f.opts.Stdout
	cmd.Stderr = f.opts.stderr()
	cmd.Stdin = os.Stdin
	var env []string
	env = append(env, os.Environ()...)
	env = append(env, "TARGET="+targetName)
	cmd.Env = env
	cmd.Dir = srcDir

	f.printf("Building binary for %s...\n", targetName)

	if err := cmd.Run(); err != nil {
		return err
	}

	// Check existing .bin files in the output directory
	obj := filepath.Join(srcDir, "obj")
	files, err := ioutil.ReadDir(obj)
	if err != nil {
		return err
	}

	var binary os.FileInfo

	for _, f := range files {
		name := f.Name()
		if filepath.Ext(name) == ".bin" {
			nonExt := name[:len(name)-4]
			// Binaries end with the target name
			if strings.HasSuffix(nonExt, targetName) {
				if binary == nil || binary.ModTime().Before(f.ModTime()) {
					binary = f
				}
			}
		}
	}
	if binary == nil {
		return fmt.Errorf("could not find binary for target %s", targetName)
	}

	binaryPath := filepath.Join(obj, binary.Name())

	f.printf("Rebooting board in DFU mode...\n")

	// Now reboot in dfu mode
	if err := f.dfuReboot(); err != nil {
		return err
	}
	if err := f.dfuWait(dfu); err != nil {
		return err
	}
	return f.dfuFlash(dfu, binaryPath)
}

// Reboots the board into the bootloader for flashing
func (f *FC) dfuReboot() error {
	_, err := f.msp.RebootIntoBootloader()
	return err
}

func (f *FC) dfuList(dfuPath string) ([]string, error) {
	cmd := exec.Command(dfuPath, "--list")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Run()
	lines := strings.Split(buf.String(), "\n")
	var dfuLines []string
	for _, ll := range lines {
		ll = strings.Trim(ll, "\n\r\t ")
		if strings.HasPrefix(ll, dfuDevicePrefix) {
			dfuLines = append(dfuLines, ll[len(dfuDevicePrefix):])
		}
	}
	return dfuLines, nil
}

func (f *FC) dfuWait(dfuPath string) error {
	timeout := time.Now().Add(30 * time.Second)
	for {
		if timeout.Before(time.Now()) {
			return fmt.Errorf("timed out while waiting for board in DFU mode")
		}
		devices, err := f.dfuList(dfuPath)
		if err != nil {
			return err
		}
		for _, dev := range devices {
			if strings.Contains(dev, internalFlashMarker) {
				// Found a flash device
				return nil
			}
		}
	}
}

func (f *FC) regexpFind(pattern string, s string) string {
	r := regexp.MustCompile(pattern)
	m := r.FindStringSubmatch(s)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

func (f *FC) dfuFlash(dfuPath string, binaryPath string) error {
	devices, err := f.dfuList(dfuPath)
	if err != nil {
		return err
	}
	var device string
	for _, dev := range devices {
		if strings.Contains(dev, internalFlashMarker) {
			device = dev
			break
		}
	}
	// a device line looks like:
	// [0483:df11] ver=2200, devnum=17, cfg=1, intf=0, path="20-1", alt=0, name="@Internal Flash  /0x08000000/04*016Kg,01*064Kg,07*128Kg", serial="3276365D3336"
	// We need to extract alt, serial and the flash offset
	alt := f.regexpFind("alt=(\\d+)", device)
	serial := f.regexpFind(`serial="(.*?)"`, device)
	offset := f.regexpFind("Internal Flash  /([\\dx]*?)/", device)
	if alt == "" || serial == "" || offset == "" {
		return fmt.Errorf("could not determine flash parameters from %q", device)
	}
	f.printf("Flashing %s via DFU to offset %s...\n", filepath.Base(binaryPath), offset)
	cmd := exec.Command(dfuPath, "-a", alt, "-S", serial, "-s", offset+":leave", "-D", binaryPath)
	cmd.Stdout = f.opts.Stdout
	cmd.Stderr = f.opts.stderr()
	return cmd.Run()
}

func (f *FC) reset() {
	f.variant = ""
	f.versionMajor = 0
	f.versionMinor = 0
	f.versionPatch = 0
	f.boardID = ""
	f.targetName = ""
	f.features = 0
}
