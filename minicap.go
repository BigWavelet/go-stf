package stf

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	adb "github.com/openatx/go-adb"
	"github.com/pkg/errors"
)

const (
	QUALITY_1080P = 1
	QUALITY_720P  = 2
	QUALITY_480P  = 3
	QUALITY_240P  = 4
)

type minicapInfo struct {
	Id       int     `json:"id"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Xdpi     float32 `json:"xdpi"`
	Ydpi     float32 `json:"ydpi"`
	Size     float32 `json:"size"`
	Density  float32 `json:"density"`
	Fps      float32 `json:"fps"`
	Secure   bool    `json:"secure"`
	Rotation int     `json:"rotation"`
}

type minicapDaemon struct {
	width, height       int
	maxWidth, maxHeight int
	rotation            int
	port                int
	quitC               chan bool
	rotationC           chan int
	binaryPath          string

	*adb.Device
	errorMixin
	safeMixin
}

func newMinicapDaemon(rotationC chan int, device *adb.Device) *minicapDaemon {
	if rotationC == nil {
		rotationC = make(chan int)
	}
	return &minicapDaemon{
		rotationC: rotationC,
		Device:    device,
		maxWidth:  720,
		maxHeight: 720,
	}
}

func (m *minicapDaemon) Start() error {
	return m.safeDo(_ACTION_START,
		func() error {
			m.resetError()
			m.quitC = make(chan bool, 1)
			m.killMinicap()
			minfo, err := m.prepareSafe()
			if err != nil {
				return errors.Wrap(err, "prepare minicap")
			}
			m.width = minfo.Width
			m.height = minfo.Height
			m.rotation = minfo.Rotation
			go m.runScreenCaptureWithRotate() // TODO
			return nil
		})
}

func (m *minicapDaemon) Stop() error {
	return m.safeDo(_ACTION_STOP,
		func() error {
			m.quitC <- true
			return m.Wait()
		})
}

// minicap may say resource is busy ..
func (m *minicapDaemon) prepareSafe() (mi minicapInfo, err error) {
	n := 0
	for {
		mi, err = m.prepare()
		if err == nil || n >= 3 {
			return
		}
		m.killMinicap()
		time.Sleep(100 * time.Millisecond)
		n += 1
	}
}

// Check whether minicap is supported on the device
// Check adb forward
// For more information, see: https://github.com/openstf/minicap
func (m *minicapDaemon) prepare() (mi minicapInfo, err error) {
	slowCap := "/data/local/tmp/slow-minicap"
	if m.isRemoteExists(slowCap) {
		m.binaryPath = slowCap
	} else {
		m.binaryPath = "/data/local/tmp/minicap"
		if err = m.pushFiles(); err != nil {
			return
		}
	}
	out, err := m.RunCommand("LD_LIBRARY_PATH=/data/local/tmp", m.binaryPath, "-i")
	if err != nil {
		err = errors.Wrap(err, "run minicap -i")
		return
	}
	err = json.Unmarshal([]byte(out), &mi)
	return
}

func (m *minicapDaemon) isRemoteExists(path string) bool {
	_, err := m.Stat(path)
	return err == nil
}

// func (m *minicapDaemon) prepareSlow() (mi minicapInfo, err error) {
// 	if err = m.pushFiles(); err != nil {
// 		return
// 	}
// 	out, err := m.RunCommand("/data/local/tmp/slow-minicap", "-i")
// 	if err != nil {
// 		err = errors.Wrap(err, "run slow-minicap -i")
// 		return
// 	}
// 	err = json.Unmarshal([]byte(out), &mi)
// 	return
// }

func (m *minicapDaemon) pushFiles() error {
	props, err := m.Properties()
	if err != nil {
		return err
	}
	abi, ok := props["ro.product.cpu.abi"]
	if !ok {
		return errors.New("No ro.product.cpu.abi propery")
	}
	sdk, ok := props["ro.build.version.sdk"]
	if !ok {
		return errors.New("No ro.build.version.sdk propery")
	}
	for _, filename := range []string{"minicap.so", "minicap"} {
		dst := "/data/local/tmp/" + filename
		if AdbFileExists(m.Device, dst) {
			continue
		}
		var urlStr string
		var perms os.FileMode = 0644
		baseUrl := "https://gohttp.nie.netease.com/openstf/vendor"
		if filename == "minicap.so" {
			urlStr = baseUrl + "/minicap/shared/android-" + sdk + "/" + abi + "/minicap.so"
		} else {
			perms = 0755
			urlStr = baseUrl + "/minicap/bin/" + abi + "/minicap"
		}
		err := PushFileFromHTTP(m.Device, dst, perms, urlStr)
		if err != nil {
			return err
		}
	}
	return nil
}

// TODO(ssx): setQuality
func (m *minicapDaemon) SetQuality(quality int) {
	switch quality {
	case QUALITY_1080P:
		m.maxHeight, m.maxHeight = 1080, 1080
	case QUALITY_720P:
		m.maxHeight, m.maxHeight = 720, 720
	case QUALITY_480P:
		m.maxHeight, m.maxHeight = 480, 480
	case QUALITY_240P:
		m.maxHeight, m.maxHeight = 240, 240
	default:
		return
	}
	m.rotationC <- m.rotation // force restart minicap
}

func (m *minicapDaemon) SetRotation(r int) {
	select {
	case m.rotationC <- r:
	case <-time.After(100 * time.Millisecond):
	}
}

func (m *minicapDaemon) runScreenCaptureWithRotate() {
	m.killMinicap()
	var err error
	defer func() {
		m.doneError(errors.Wrap(err, "minicap"))
	}()
	errC := GoFunc(m.runScreenCapture)
	var needRestart bool
	for {
		select {
		case err = <-errC: // when normal exit, that is an error
			if !needRestart {
				return
			}
			needRestart = false
			err = nil
			errC = GoFunc(m.runScreenCapture)
		case r := <-m.rotationC:
			needRestart = true
			m.rotation = r
			m.killMinicap()
		case <-m.quitC:
			m.killMinicap()
			return
		}
	}
}

func (m *minicapDaemon) runScreenCapture() (err error) {
	param := fmt.Sprintf("%dx%d@%dx%d/%d", m.width, m.height, m.maxWidth, m.maxHeight, m.rotation)
	c, err := m.OpenCommand("LD_LIBRARY_PATH=/data/local/tmp", m.binaryPath, "-P", param, "-S")
	if err != nil {
		return
	}
	defer c.Close()
	buf := bufio.NewReader(c)

	// Example output below --.
	// PID: 9355
	// INFO: Using projection 720x1280@720x1280/0
	// INFO: (jni/minicap/JpgEncoder.cpp:64) Allocating 2766852 bytes for JPG encoder
	line, _, err := buf.ReadLine()
	if err != nil {
		return
	}
	if !strings.Contains(string(line), "PID:") {
		err = errors.New("expect PID: <pid> actually: " + strconv.Quote(string(line)))
		return errors.Wrap(err, "run minicap")
	}
	for {
		_, _, err = buf.ReadLine()
		if err != nil {
			break
		}
	}
	return errors.New("minicap quit")
}

func (m *minicapDaemon) killMinicap() error {
	m.killProc("minicap", syscall.SIGKILL)
	m.killProc("slow-minicap", syscall.SIGKILL)
	return nil
}

// FIXME(ssx): maybe need to put into go-adb
func (m *minicapDaemon) killProc(psName string, sig syscall.Signal) (err error) {
	out, err := m.RunCommand("ps", "-C", psName)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) <= 1 {
		return errors.New("No process named " + psName + " founded.")
	}
	var pidIndex int
	for idx, val := range strings.Fields(lines[0]) {
		if val == "PID" {
			pidIndex = idx
			break
		}
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if !strings.Contains(line, psName) {
			continue
		}
		pid := fields[pidIndex]
		m.RunCommand("kill", "-"+strconv.Itoa(int(sig)), pid)
	}
	return
}

type jpgTcpSucker struct {
	port        int
	conn        net.Conn
	quitC       chan bool
	C           chan []byte
	forwardSpec adb.ForwardSpec

	errorMixin
	safeMixin
	*adb.Device
}

func (s *jpgTcpSucker) Start() error {
	return s.safeDo(_ACTION_START, func() error {
		s.resetError()
		var err error
		s.C = make(chan []byte, 3)
		s.quitC = make(chan bool, 1)
		s.port, err = s.prepareForward()
		if err != nil {
			return err
		}
		go s.keepReadFromTcp()
		return nil
	})
}

func (s *jpgTcpSucker) Stop() error {
	return s.safeDo(_ACTION_STOP, func() error {
		s.quitC <- true
		if s.conn != nil {
			s.conn.Close()
		}
		return s.Wait()
	})
}

// adb forward tcp:{port} localabstract:minicap
func (s *jpgTcpSucker) prepareForward() (port int, err error) {
	return s.ForwardToFreePort(s.forwardSpec) //adb.ForwardSpec{adb.FProtocolTcp, "2016"})
	// return s.ForwardToFreePort(adb.ForwardSpec{adb.FProtocolAbstract, "minicap"})
}

type errorBinaryReader struct {
	rd  io.Reader
	err error
}

func (r *errorBinaryReader) ReadInto(datas ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	for _, data := range datas {
		r.err = binary.Read(r.rd, binary.LittleEndian, data)
		if r.err != nil {
			return r.err
		}
	}
	return nil
}

// TODO(ssx): Do not add retry for now
func (s *jpgTcpSucker) keepReadFromTcp() (err error) {
	defer func() {
		s.doneError(errors.Wrap(err, "readFromTcp"))
	}()
	leftRetry := 10
	for {
		select {
		case err = <-GoFunc(s.readFromTcp):
		case <-s.quitC:
			return nil
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-s.quitC:
			return nil
		}
		if leftRetry <= 0 {
			err = errors.New("jpgTcpSucker reach max retry(10)")
			return
		}
		leftRetry -= 1
	}
}

func (s *jpgTcpSucker) readFromTcp() (err error) {
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(s.port))
	if err != nil {
		return
	}
	s.conn = conn
	defer conn.Close()

	var pid, rw, rh, vw, vh uint32
	var version, unused, orientation, quirkFlag uint8

	rd := bufio.NewReader(conn)
	binRd := errorBinaryReader{rd: rd}
	err = binRd.ReadInto(&version, &unused, &pid, &rw, &rh, &vw, &vh, &orientation, &quirkFlag)
	if err != nil {
		return err
	}

	for {
		var size uint32
		if err = binRd.ReadInto(&size); err != nil {
			break
		}

		lr := &io.LimitedReader{rd, int64(size)}
		buf := bytes.NewBuffer(nil)
		_, err = io.Copy(buf, lr)
		if err != nil {
			break
		}
		if string(buf.Bytes()[:2]) != "\xff\xd8" {
			err = errors.New("jpeg format error, not starts with 0xff,0xd8")
			break
		}
		select {
		case s.C <- buf.Bytes(): // Maybe should use buffer instead
		default:
			// image should not wait or it will stuck here
		}
	}
	return err
}

type STFCapturer struct {
	*minicapDaemon
	*jpgTcpSucker
}

func NewSTFCapturer(device *adb.Device) *STFCapturer {
	return &STFCapturer{
		minicapDaemon: newMinicapDaemon(nil, device),
		jpgTcpSucker:  &jpgTcpSucker{Device: device},
	}
}

func (s *STFCapturer) Start() error {
	err := s.minicapDaemon.Start()
	if err != nil {
		return err
	}
	if s.minicapDaemon.binaryPath == "/data/local/tmp/slow-minicap" {
		s.jpgTcpSucker.forwardSpec = adb.ForwardSpec{adb.FProtocolTcp, "2016"}
	} else {
		s.jpgTcpSucker.forwardSpec = adb.ForwardSpec{adb.FProtocolAbstract, "minicap"}
	}
	return s.jpgTcpSucker.Start()
}

func (s *STFCapturer) Stop() error {
	return wrapMultiError(
		s.minicapDaemon.Stop(),
		s.jpgTcpSucker.Stop())
}

func (s *STFCapturer) Wait() error {
	select {
	case err := <-GoFunc(s.minicapDaemon.Wait):
		return err
	case err := <-GoFunc(s.jpgTcpSucker.Wait):
		return err
	}
	// return wrapMultiError(
	// 	s.minicapDaemon.Wait(),
	// 	s.jpgTcpSucker.Wait())
}
