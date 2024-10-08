package deej

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jacobsa/go-serial/serial"
	"go.uber.org/zap"

	"github.com/micmonay/keybd_event"

	"github.com/omriharel/deej/pkg/deej/util"
)

// SerialIO provides a deej-aware abstraction layer to managing serial I/O
type SerialIO struct {
	comPort  string
	baudRate uint

	deej   *Deej
	logger *zap.SugaredLogger

	stopChannel chan bool
	connected   bool
	connOptions serial.OpenOptions
	conn        io.ReadWriteCloser

	lastKnownNumSliders        int
	currentSliderPercentValues []float32
	lastKnownNumButtons        int
	currentButtonValues        []int

	sliderMoveConsumers []chan SliderMoveEvent
	buttonMoveConsumers []chan ButtonPressEvent
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
}

type ButtonPressEvent struct {
	ButtonID      int
	PreviousValue int
	ButtonValue   int
}

var expectedLinePattern = regexp.MustCompile(`^\d{1,4}(\|\d{1,4})*\r\n$`)
var buttonLinePattern = regexp.MustCompile(`^~\d(\~\d)*~\r\n$`) // ~1~ or ~0~ for 1 button values

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with the arduino chip
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

	sio := &SerialIO{
		deej:                deej,
		logger:              logger,
		stopChannel:         make(chan bool),
		connected:           false,
		conn:                nil,
		sliderMoveConsumers: []chan SliderMoveEvent{},
		buttonMoveConsumers: []chan ButtonPressEvent{},
	}

	logger.Debug("Created serial i/o instance")

	// respond to config changes
	sio.setupOnConfigReload()

	return sio, nil
}

// Start attempts to connect to our arduino chip
func (sio *SerialIO) Start() error {

	// don't allow multiple concurrent connections
	if sio.connected {
		sio.logger.Warn("Already connected, can't start another without closing first")
		return errors.New("serial: connection already active")
	}

	// set minimum read size according to platform (0 for windows, 1 for linux)
	// this prevents a rare bug on windows where serial reads get congested,
	// resulting in significant lag
	minimumReadSize := 0
	if util.Linux() {
		minimumReadSize = 1
	}

	sio.connOptions = serial.OpenOptions{
		PortName:        sio.deej.config.ConnectionInfo.COMPort,
		BaudRate:        uint(sio.deej.config.ConnectionInfo.BaudRate),
		DataBits:        8,
		StopBits:        1,
		MinimumReadSize: uint(minimumReadSize),
	}

	sio.logger.Debugw("Attempting serial connection",
		"comPort", sio.connOptions.PortName,
		"baudRate", sio.connOptions.BaudRate,
		"minReadSize", minimumReadSize)

	var err error
	sio.conn, err = serial.Open(sio.connOptions)
	if err != nil {

		// might need a user notification here, TBD
		sio.logger.Warnw("Failed to open serial connection", "error", err)
		return fmt.Errorf("open serial connection: %w", err)
	}

	namedLogger := sio.logger.Named(strings.ToLower(sio.connOptions.PortName))

	namedLogger.Infow("Connected", "conn", sio.conn)
	sio.connected = true

	// read lines or await a stop
	go func() {
		connReader := bufio.NewReader(sio.conn)
		lineChannel := sio.readLine(namedLogger, connReader)

		for {
			select {
			case <-sio.stopChannel:
				sio.close(namedLogger)
			case line := <-lineChannel:
				sio.handleLine(namedLogger, line)
			}
		}
	}()

	return nil
}

// Stop signals us to shut down our serial connection, if one is active
func (sio *SerialIO) Stop() {
	if sio.connected {
		sio.logger.Debug("Shutting down serial connection")
		sio.stopChannel <- true
	} else {
		sio.logger.Debug("Not currently connected, nothing to stop")
	}
}

// SubscribeToSliderMoveEvents returns an unbuffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (sio *SerialIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent)
	sio.sliderMoveConsumers = append(sio.sliderMoveConsumers, ch)

	return ch
}

func (sio *SerialIO) setupOnConfigReload() {
	configReloadedChannel := sio.deej.config.SubscribeToChanges()

	const stopDelay = 50 * time.Millisecond

	go func() {
		for {
			select {
			case <-configReloadedChannel:

				// make any config reload unset our slider number to ensure process volumes are being re-set
				// (the next read line will emit SliderMoveEvent instances for all sliders)\
				// this needs to happen after a small delay, because the session map will also re-acquire sessions
				// whenever the config file is reloaded, and we don't want it to receive these move events while the map
				// is still cleared. this is kind of ugly, but shouldn't cause any issues
				go func() {
					<-time.After(stopDelay)
					sio.lastKnownNumSliders = 0
					sio.lastKnownNumButtons = 0
				}()

				// if connection params have changed, attempt to stop and start the connection
				if sio.deej.config.ConnectionInfo.COMPort != sio.connOptions.PortName ||
					uint(sio.deej.config.ConnectionInfo.BaudRate) != sio.connOptions.BaudRate {

					sio.logger.Info("Detected change in connection parameters, attempting to renew connection")
					sio.Stop()

					// let the connection close
					<-time.After(stopDelay)

					if err := sio.Start(); err != nil {
						sio.logger.Warnw("Failed to renew connection after parameter change", "error", err)
					} else {
						sio.logger.Debug("Renewed connection successfully")
					}
				}
			}
		}
	}()
}

func (sio *SerialIO) close(logger *zap.SugaredLogger) {
	if err := sio.conn.Close(); err != nil {
		logger.Warnw("Failed to close serial connection", "error", err)
	} else {
		logger.Debug("Serial connection closed")
	}

	sio.conn = nil
	sio.connected = false
}

func (sio *SerialIO) readLine(logger *zap.SugaredLogger, reader *bufio.Reader) chan string {
	ch := make(chan string)

	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {

				if sio.deej.Verbose() {
					logger.Warnw("Failed to read line from serial", "error", err, "line", line)
				}

				// just ignore the line, the read loop will stop after this
				return
			}

			if sio.deej.Verbose() {
				logger.Debugw("Read new line", "line", line)
			}

			// deliver the line to the channel
			ch <- line
		}
	}()

	return ch
}

var KEY_MAPS = map[string]int{
	// https://github.com/micmonay/keybd_event/blob/master/keybd_windows.go
	"VK_MEDIA_NEXT_TRACK":    keybd_event.VK_MEDIA_NEXT_TRACK,
	"VK_MEDIA_PREV_TRACK":    keybd_event.VK_MEDIA_PREV_TRACK,
	"VK_MEDIA_STOP":          keybd_event.VK_MEDIA_STOP,
	"VK_MEDIA_PLAY_PAUSE":    keybd_event.VK_MEDIA_PLAY_PAUSE,
	"VK_LAUNCH_MEDIA_SELECT": keybd_event.VK_LAUNCH_MEDIA_SELECT,
	"VK_VOLUME_MUTE":         keybd_event.VK_VOLUME_MUTE,
	"VK_VOLUME_DOWN":         keybd_event.VK_VOLUME_DOWN,
	"VK_VOLUME_UP":           keybd_event.VK_VOLUME_UP,
	"VK_BROWSER_BACK":        keybd_event.VK_BROWSER_BACK,
	"VK_BROWSER_FORWARD":     keybd_event.VK_BROWSER_FORWARD,
	"VK_BROWSER_REFRESH":     keybd_event.VK_BROWSER_REFRESH,
	"VK_BROWSER_STOP":        keybd_event.VK_BROWSER_STOP,
	"VK_BROWSER_SEARCH":      keybd_event.VK_BROWSER_SEARCH,
	"VK_BROWSER_FAVORITES":   keybd_event.VK_BROWSER_FAVORITES,
	"VK_BROWSER_HOME":        keybd_event.VK_BROWSER_HOME,
}

func (sio *SerialIO) kbKeySimple(kb *keybd_event.KeyBonding, key string) error {

	key_data, exists := KEY_MAPS[key]
	if !exists {
		return errors.New("Key not found")
	}

	kb.SetKeys(key_data)

	return nil
}

func (sio *SerialIO) pressedButton(logger *zap.SugaredLogger, buttonEvent ButtonPressEvent) {
	bindex := buttonEvent.ButtonID
	logger.Debugw("pressedButton", "event", buttonEvent, "ButtonMapping.m[bindex]", sio.deej.config.ButtonMapping.m[bindex])

	kb, err := keybd_event.NewKeyBonding()
	if err != nil {
		panic(err)
	}

	for conf_ind, conf_key := range sio.deej.config.ButtonMapping.m[bindex] {
		// logger.Debugw("pressedButton",
		// 	"conf_ind", conf_ind,
		// 	"conf_key", conf_key,
		// )
		// https://github.com/micmonay/keybd_event/blob/master/keybd_windows.go#L281
		// send_key := "VK_MEDIA_PLAY_PAUSE"
		// KEY_MAPS

		key_err := err
		if conf_key == "FORCE_REFRESH" {
			kb.SetKeys(keybd_event.VK_F5)
			kb.HasCTRL(true)
		} else if conf_key == "WIN_MIC_MUTE_TOGGLE" {
			kb.SetKeys(keybd_event.VK_K)
			kb.HasSuper(true)
			kb.HasALTGR(true)
		} else {
			key_err = sio.kbKeySimple(&kb, conf_key)
			// logger.Debugw("kbKeySimple", "key_err", key_err)
		}

		if key_err != nil {
			logger.Debugw("pressedButton invalid key",
				"conf_ind", conf_ind,
				"conf_key", conf_key,
				"key_err", key_err,
			)
		}

	}

	// Press the selected keys
	err = kb.Launching()
	if err != nil {
		panic(err)
	}
}

func (sio *SerialIO) handleButtons(logger *zap.SugaredLogger, line string) {

	// trim the suffix
	line = strings.TrimSuffix(line, "\r\n")
	line = strings.TrimSuffix(line, "~")
	line = strings.Trim(line, "~")

	// logger.Debugw("raw button", "event", line)

	// split on ~, this gives a slice of numerical strings between "0" and "9"
	splitLine := strings.Split(line, "~")
	numSliders := len(splitLine)

	// logger.Debugw("raw button data",
	// 	"splitLine", splitLine,
	// 	"numSliders", numSliders,
	// )

	// update our slider count, if needed - this will send slider move events for all
	if numSliders != sio.lastKnownNumButtons {
		logger.Infow("Detected buttons", "amount", numSliders)
		sio.lastKnownNumButtons = numSliders
		sio.currentButtonValues = make([]int, numSliders)

		// reset everything to be an impossible value to force the slider move event later
		for idx := range sio.currentButtonValues {
			sio.currentButtonValues[idx] = -1.0
		}
	}

	// for each slider:
	moveEvents := []ButtonPressEvent{}
	for sliderIdx, stringValue := range splitLine {

		// convert string values to integers ("1023" -> 1023)
		number, _ := strconv.Atoi(stringValue)
		number = int(number)

		// turns out the first line could come out dirty sometimes (i.e. "4558|925|41|643|220")
		// so let's check the first number for correctness just in case
		if sliderIdx == 0 && number > 9 {
			sio.logger.Debugw("Got malformed line from serial, ignoring", "line", line)
			return
		}

		// logger.Debugw("button info",
		// 	"sliderIdx", sliderIdx,
		// 	"stringValue", stringValue,
		// 	"number", number,
		// )

		// check if it changes the desired state (could just be a jumpy raw slider value)
		if sio.currentButtonValues[sliderIdx] != number {

			moveEvents = append(moveEvents, ButtonPressEvent{
				ButtonID:      sliderIdx,
				PreviousValue: sio.currentButtonValues[sliderIdx],
				ButtonValue:   number,
			})

			sio.currentButtonValues[sliderIdx] = number

			if sio.deej.Verbose() {
				logger.Debugw("Button state changed", "event", moveEvents[len(moveEvents)-1])
			}
		}
	}

	for _, moveEvent := range moveEvents {
		if moveEvent.PreviousValue == 0 && moveEvent.ButtonValue != 0 {
			sio.pressedButton(logger, moveEvent)
		}
	}

	// TODO not properly implemented !!!!!!!
	// // deliver move events if there are any, towards all potential consumers
	// if len(moveEvents) > 0 {
	// 	for _, consumer := range sio.buttonMoveConsumers {
	// 		for _, moveEvent := range moveEvents {
	// 			consumer <- moveEvent
	// 		}
	// 	}
	// }

	return
}

func (sio *SerialIO) handleLine(logger *zap.SugaredLogger, line string) {

	if buttonLinePattern.MatchString(line) {
		sio.handleButtons(logger, line)
		return
	}

	// this function receives an unsanitized line which is guaranteed to end with LF,
	// but most lines will end with CRLF. it may also have garbage instead of
	// deej-formatted values, so we must check for that! just ignore bad ones
	if !expectedLinePattern.MatchString(line) {
		return
	}

	// trim the suffix
	line = strings.TrimSuffix(line, "\r\n")

	// split on pipe (|), this gives a slice of numerical strings between "0" and "1023"
	splitLine := strings.Split(line, "|")
	numSliders := len(splitLine)

	// update our slider count, if needed - this will send slider move events for all
	if numSliders != sio.lastKnownNumSliders {
		logger.Infow("Detected sliders", "amount", numSliders)
		sio.lastKnownNumSliders = numSliders
		sio.currentSliderPercentValues = make([]float32, numSliders)

		// reset everything to be an impossible value to force the slider move event later
		for idx := range sio.currentSliderPercentValues {
			sio.currentSliderPercentValues[idx] = -1.0
		}
	}

	// for each slider:
	moveEvents := []SliderMoveEvent{}
	for sliderIdx, stringValue := range splitLine {

		// convert string values to integers ("1023" -> 1023)
		number, _ := strconv.Atoi(stringValue)

		// turns out the first line could come out dirty sometimes (i.e. "4558|925|41|643|220")
		// so let's check the first number for correctness just in case
		if sliderIdx == 0 && number > 1023 {
			sio.logger.Debugw("Got malformed line from serial, ignoring", "line", line)
			return
		}

		// map the value from raw to a "dirty" float between 0 and 1 (e.g. 0.15451...)
		dirtyFloat := float32(number) / 1023.0

		// normalize it to an actual volume scalar between 0.0 and 1.0 with 2 points of precision
		normalizedScalar := util.NormalizeScalar(dirtyFloat)

		// if sliders are inverted, take the complement of 1.0
		if sio.deej.config.InvertSliders {
			normalizedScalar = 1 - normalizedScalar
		}

		// check if it changes the desired state (could just be a jumpy raw slider value)
		if util.SignificantlyDifferent(sio.currentSliderPercentValues[sliderIdx], normalizedScalar, sio.deej.config.NoiseReductionLevel) {

			// if it does, update the saved value and create a move event
			sio.currentSliderPercentValues[sliderIdx] = normalizedScalar

			moveEvents = append(moveEvents, SliderMoveEvent{
				SliderID:     sliderIdx,
				PercentValue: normalizedScalar,
			})

			if sio.deej.Verbose() {
				logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
			}
		}
	}

	// deliver move events if there are any, towards all potential consumers
	if len(moveEvents) > 0 {
		for _, consumer := range sio.sliderMoveConsumers {
			for _, moveEvent := range moveEvents {
				consumer <- moveEvent
			}
		}
	}
}
