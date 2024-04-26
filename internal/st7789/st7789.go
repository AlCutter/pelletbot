// Package st7789 implements a driver for the ST7789 TFT displays, it comes in various screen sizes.
//
// Datasheets: https://cdn-shop.adafruit.com/product-files/3787/3787_tft_QT154H2201__________20190228182902.pdf
//
//	http://www.newhavendisplay.com/appnotes/datasheets/LCDs/ST7789V.pdf
package st7789

import (
	"errors"
	"fmt"
	"image/color"
	"math"
	"time"

	"github.com/AlCutter/pelletbot/internal/st7789/pixel"

	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/physic"
	"periph.io/x/conn/v3/spi"
)

// The color format used on the display, like RGB565, RGB666, and RGB444.
type ColorFormat uint8

// Pixel formats supported by the st7789 driver.
type Color interface {
	pixel.RGB444BE | pixel.RGB565BE

	pixel.BaseColor
}

// FrameRate controls the frame rate used by the display.
type FrameRate uint8

var (
	errOutOfBounds = errors.New("rectangle coordinates outside display area")
)

// Device wraps an SPI connection.
type Device = DeviceOf[pixel.RGB565BE]

// DeviceOf is a generic version of Device. It supports multiple different pixel
// formats.
type DeviceOf[T Color] struct {
	// Communication
	c               spi.Conn
	dcPin           gpio.PinOut
	resetPin        gpio.PinOut
	backlight       gpio.PinIO
	width           int16
	height          int16
	columnOffsetCfg int16
	rowOffsetCfg    int16
	columnOffset    int16
	rowOffset       int16
	rotation        Rotation
	frameRate       FrameRate
	batchLength     int32
	batchData       pixel.Image[T] // "image" with (width, height) of (batchLength, 1)
	isBGR           bool
	vSyncLines      int16
	cmdBuf          [1]byte
	buf             [6]byte
}

// Config is the configuration for the display
type Config struct {
	Width        int16
	Height       int16
	Rotation     Rotation
	RowOffset    int16
	ColumnOffset int16
	FrameRate    FrameRate
	VSyncLines   int16

	// Gamma control. Look in the LCD panel datasheet or provided example code
	// to find these values. If not set, the defaults will be used.
	PVGAMCTRL []uint8 // Positive voltage gamma control (14 bytes)
	NVGAMCTRL []uint8 // Negative voltage gamma control (14 bytes)
}

func New(p spi.Port, dc gpio.PinOut) (*Device, error) {
	return NewOf[pixel.RGB565BE](p, dc)
}

// NewOf creates a new ST7789 connection with a particular pixel format. The SPI
// wire must already be configured.
func NewOf[T Color](p spi.Port, dc gpio.PinOut) (*DeviceOf[T], error) {
	if dc == gpio.INVALID {
		return nil, errors.New("ssd1306: use nil for dc to use 3-wire mode, do not use gpio.INVALID")
	}
	bits := 8
	if err := dc.Out(gpio.Low); err != nil {
		return nil, err
	}
	c, err := p.Connect(80*physic.MegaHertz, spi.Mode0, bits)
	if err != nil {
		return nil, err
	}

	/*
			    # User buttons
		    BUTTON_A = 5
		    BUTTON_B = 6
		    BUTTON_X = 16
		    BUTTON_Y = 24

		    # Onboard RGB LED
		    LED_R = 17
		    LED_G = 27
		    LED_B = 22
	*/

	blPin := gpioreg.ByName("GPIO13")
	if err = blPin.Out(gpio.Low); err != nil {
		panic(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err = blPin.Out(gpio.High); err != nil {
		panic(err)
	}

	d := &DeviceOf[T]{
		c:         c,
		dcPin:     dc,
		backlight: blPin,
	}

	return d, nil
}

// Configure initializes the display with default configuration
func (d *DeviceOf[T]) Configure(cfg Config) {
	if cfg.Width != 0 {
		d.width = cfg.Width
	} else {
		d.width = 240
	}
	if cfg.Height != 0 {
		d.height = cfg.Height
	} else {
		d.height = 240
	}

	d.rotation = cfg.Rotation
	d.rowOffsetCfg = cfg.RowOffset
	d.columnOffsetCfg = cfg.ColumnOffset

	if cfg.FrameRate != 0 {
		d.frameRate = cfg.FrameRate
	} else {
		d.frameRate = FRAMERATE_60
	}

	if cfg.VSyncLines >= 2 && cfg.VSyncLines <= 254 {
		d.vSyncLines = cfg.VSyncLines
	} else {
		d.vSyncLines = 16
	}

	d.batchLength = int32(d.width)
	if d.height > d.width {
		d.batchLength = int32(d.height)
	}
	d.batchLength += d.batchLength & 1

	// Common initialization
	d.startWrite()
	d.sendCommand(SWRESET, nil) // Soft reset
	d.endWrite()
	time.Sleep(150 * time.Millisecond) //
	d.startWrite()

	d.sendCommand(SLPOUT, nil) // Exit sleep mode

	// Memory initialization
	var zeroColor T
	switch any(zeroColor).(type) {
	case pixel.RGB444BE:
		d.setColorFormat(ColorRGB444) // 12 bits per pixel
	default:
		// Use default RGB565 color format.
		d.setColorFormat(ColorRGB565) // 16 bits per pixel
	}
	time.Sleep(10 * time.Millisecond)

	d.setRotation(d.rotation) // Memory orientation

	d.setWindow(0, 0, d.width, d.height)   // Full draw window
	d.fillScreen(color.RGBA{0, 0, 0, 255}) // Clear screen

	// Framerate
	d.sendCommand(FRCTRL2, []byte{byte(d.frameRate)}) // Frame rate for normal mode (default 60Hz)

	// Frame vertical sync and "porch"
	//
	// Front and back porch controls vertical scanline sync time before and after
	// a frame, where memory can be safely written without tearing.
	//
	fp := uint8(d.vSyncLines / 2)         // Split the desired pause half and half
	bp := uint8(d.vSyncLines - int16(fp)) // between front and back porch.

	d.sendCommand(PORCTRL, []byte{
		bp,   // Back porch 5bit     (0x7F max 0x08 default)
		fp,   // Front porch 5bit    (0x7F max 0x08 default)
		0x00, // Seprarate porch     (TODO: what is this?)
		0x22, // Idle mode porch     (4bit-back 4bit-front 0x22 default)
		0x22, // Partial mode porch  (4bit-back 4bit-front 0x22 default)
	})

	// Ready to display
	d.sendCommand(INVON, nil)         // Inversion ON
	time.Sleep(10 * time.Millisecond) //

	// Set gamma tables, if configured.
	if len(cfg.PVGAMCTRL) == 14 {
		d.sendCommand(GMCTRP1, cfg.PVGAMCTRL) // PVGAMCTRL: Positive Voltage Gamma Control
	}
	if len(cfg.NVGAMCTRL) == 14 {
		d.sendCommand(GMCTRN1, cfg.NVGAMCTRL) // NVGAMCTRL: Negative Voltage Gamma Control
	}

	d.sendCommand(NORON, nil)         // Normal mode ON
	time.Sleep(10 * time.Millisecond) //

	d.sendCommand(RAMCTRL, []byte{0x00, 0xE8})

	d.sendCommand(DISPON, nil)        // Screen ON
	time.Sleep(10 * time.Millisecond) //

	d.endWrite()
	d.backlight.Out(gpio.High) // Backlight ON
}

// Send a command with data to the display. It does not change the chip select
// pin (it must be low when calling). The DC pin is left high after return,
// meaning that data can be sent right away.
func (d *DeviceOf[T]) sendCommand(command uint8, data []byte) error {
	d.cmdBuf[0] = command
	d.cmdBuf[0] = command
	d.dcPin.Out(gpio.Low)
	err := d.c.Tx(d.cmdBuf[:1], nil)
	d.dcPin.Out(gpio.High)
	if len(data) != 0 {
		err = d.c.Tx(data, nil)
	}
	return err
	/*
		p := spi.Packet{
			W: append([]byte{command}, data...),
		}
		d.dcPin.Out(gpio.Low)
		if err := d.c.TxPackets([]spi.Packet{p}); err != nil {
			return nil, err
		}
	*/
}

// startWrite must be called at the beginning of all exported methods to set the
// chip select pin low.
func (d *DeviceOf[T]) startWrite() {
}

// endWrite must be called at the end of all exported methods to set the chip
// select pin high.
func (d *DeviceOf[T]) endWrite() {
}

// getBuffer returns the image buffer, that's always d.batchLength wide and 1
// pixel high. It can be used as a temporary buffer to transmit image data.
func (d *DeviceOf[T]) getBuffer() pixel.Image[T] {
	if d.batchData.Len() == 0 {
		d.batchData = pixel.NewImage[T](int(d.batchLength), 1)
	}
	return d.batchData
}

// Sync waits for the display to hit the next VSYNC pause
func (d *DeviceOf[T]) Sync() {
	d.SyncToScanLine(0)
}

// SyncToScanLine waits for the display to hit a specific scanline
//
// A scanline value of 0 will forward to the beginning of the next VSYNC,
// even if the display is currently in a VSYNC pause.
//
// Syncline values appear to increment once for every two vertical
// lines on the display.
//
// NOTE: Use GetHighestScanLine and GetLowestScanLine to obtain the highest
// and lowest useful values. Values are affected by front and back porch
// vsync settings (derived from VSyncLines configuration option).
func (d *DeviceOf[T]) SyncToScanLine(scanline uint16) {
	scan := d.GetScanLine()

	// Sometimes GetScanLine returns erroneous 0 on first call after draw, so double check
	if scan == 0 {
		scan = d.GetScanLine()
	}

	if scanline == 0 {
		// we dont know where we are in an ongoing vsync so go around
		for scan < 1 {
			time.Sleep(1 * time.Millisecond)
			scan = d.GetScanLine()
		}
		for scan > 0 {
			scan = d.GetScanLine()
		}
	} else {
		// go around unless we're very close to the target
		for scan > scanline+4 {
			time.Sleep(1 * time.Millisecond)
			scan = d.GetScanLine()
		}
		for scan < scanline {
			scan = d.GetScanLine()
		}
	}
}

// GetScanLine reads the current scanline value from the display
func (d *DeviceOf[T]) GetScanLine() uint16 {
	d.startWrite()
	err := d.sendCommand(GSCAN, nil)
	if err != nil {
		fmt.Printf("GSCAN: %v", err)
		return 0
	}
	r := make([]byte, 4)
	if err := d.c.Tx(nil, r); err != nil {
		fmt.Printf("Tx: %v", err)
		return 0

	}
	scanline := uint16(r[0])<<8 + uint16(r[1])
	d.endWrite()
	return scanline
}

// GetHighestScanLine calculates the last scanline id in the frame before VSYNC pause
func (d *DeviceOf[T]) GetHighestScanLine() uint16 {
	// Last scanline id appears to be backporch/2 + 320/2
	return uint16(math.Ceil(float64(d.vSyncLines)/2)/2) + 160
}

// GetLowestScanLine calculate the first scanline id to appear after VSYNC pause
func (d *DeviceOf[T]) GetLowestScanLine() uint16 {
	// First scanline id appears to be backporch/2 + 1
	return uint16(math.Ceil(float64(d.vSyncLines)/2)/2) + 1
}

// Display does nothing, there's no buffer as it might be too big for some boards
func (d *DeviceOf[T]) Display() error {
	return nil
}

// SetPixel sets a pixel in the screen
func (d *DeviceOf[T]) SetPixel(x int16, y int16, c color.RGBA) {
	if x < 0 || y < 0 ||
		(((d.rotation == NO_ROTATION || d.rotation == ROTATION_180) && (x >= d.width || y >= d.height)) ||
			((d.rotation == ROTATION_90 || d.rotation == ROTATION_270) && (x >= d.height || y >= d.width))) {
		return
	}
	d.FillRectangle(x, y, 1, 1, c)
}

// setWindow prepares the screen to be modified at a given rectangle
func (d *DeviceOf[T]) setWindow(x, y, w, h int16) {
	x += d.columnOffset
	y += d.rowOffset
	copy(d.buf[:4], []uint8{uint8(x >> 8), uint8(x), uint8((x + w - 1) >> 8), uint8(x + w - 1)})
	d.sendCommand(CASET, d.buf[:4])
	copy(d.buf[:4], []uint8{uint8(y >> 8), uint8(y), uint8((y + h - 1) >> 8), uint8(y + h - 1)})
	d.sendCommand(RASET, d.buf[:4])
	d.sendCommand(RAMWR, nil)
}

// FillRectangle fills a rectangle at a given coordinates with a color
func (d *DeviceOf[T]) FillRectangle(x, y, width, height int16, c color.RGBA) error {
	d.startWrite()
	err := d.fillRectangle(x, y, width, height, c)
	d.endWrite()
	return err
}

func (d *DeviceOf[T]) fillRectangle(x, y, width, height int16, c color.RGBA) error {
	k, i := d.Size()
	if x < 0 || y < 0 || width <= 0 || height <= 0 ||
		x >= k || (x+width) > k || y >= i || (y+height) > i {
		return errors.New("rectangle coordinates outside display area")
	}
	d.setWindow(x, y, width, height)

	image := d.getBuffer()
	image.FillSolidColor(pixel.NewColor[T](c.R, c.G, c.B))
	j := int(width) * int(height)
	for j > 0 {
		// The DC pin is already set to data in the setWindow call, so we can
		// just write bytes on the SPI bus.
		if j >= image.Len() {
			d.c.Tx(image.RawBuffer(), nil)
		} else {
			d.c.Tx(image.Rescale(j, 1).RawBuffer(), nil)
		}
		j -= image.Len()
	}
	return nil
}

// DrawRGBBitmap8 copies an RGB bitmap to the internal buffer at given coordinates
//
// Deprecated: use DrawBitmap instead.
func (d *DeviceOf[T]) DrawRGBBitmap8(x, y int16, data []uint8, w, h int16) error {
	k, i := d.Size()
	if x < 0 || y < 0 || w <= 0 || h <= 0 ||
		x >= k || (x+w) > k || y >= i || (y+h) > i {
		return errOutOfBounds
	}
	d.startWrite()
	d.setWindow(x, y, w, h)
	var err error
	for i, t := 0, len(data); i < t; {
		r := t - i
		if r > int(d.batchLength) {
			r = int(d.batchLength)
		}

		err = d.c.Tx(data[i:i+r], nil)
		i += r
	}
	d.endWrite()
	return err
}

// DrawBitmap copies the bitmap to the internal buffer on the screen at the
// given coordinates. It returns once the image data has been sent completely.
func (d *DeviceOf[T]) DrawBitmap(x, y int16, bitmap pixel.Image[T]) error {
	width, height := bitmap.Size()
	return d.DrawRGBBitmap8(x, y, bitmap.RawBuffer(), int16(width), int16(height))
}

// FillRectangleWithBuffer fills buffer with a rectangle at a given coordinates.
func (d *DeviceOf[T]) FillRectangleWithBuffer(x, y, width, height int16, buffer []color.RGBA) error {
	i, j := d.Size()
	if x < 0 || y < 0 || width <= 0 || height <= 0 ||
		x >= i || (x+width) > i || y >= j || (y+height) > j {
		return errors.New("rectangle coordinates outside display area")
	}
	if int32(width)*int32(height) != int32(len(buffer)) {
		return errors.New("buffer length does not match with rectangle size")
	}
	d.startWrite()
	d.setWindow(x, y, width, height)

	k := int(width) * int(height)
	image := d.getBuffer()
	offset := 0
	for k > 0 {
		for i := 0; i < image.Len(); i++ {
			if offset+i < len(buffer) {
				c := buffer[offset+i]
				image.Set(i, 0, pixel.NewColor[T](c.R, c.G, c.B))
			}
		}
		// The DC pin is already set to data in the setWindow call, so we don't
		// have to set it here.
		if k >= image.Len() {
			d.c.Tx(image.RawBuffer(), nil)
		} else {
			d.c.Tx(image.Rescale(k, 1).RawBuffer(), nil)
		}
		k -= image.Len()
		offset += image.Len()
	}
	d.endWrite()
	return nil
}

// DrawFastVLine draws a vertical line faster than using SetPixel
func (d *DeviceOf[T]) DrawFastVLine(x, y0, y1 int16, c color.RGBA) {
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	d.FillRectangle(x, y0, 1, y1-y0+1, c)
}

// DrawFastHLine draws a horizontal line faster than using SetPixel
func (d *DeviceOf[T]) DrawFastHLine(x0, x1, y int16, c color.RGBA) {
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	d.FillRectangle(x0, y, x1-x0+1, 1, c)
}

// FillScreen fills the screen with a given color
func (d *DeviceOf[T]) FillScreen(c color.RGBA) {
	d.startWrite()
	d.fillScreen(c)
	d.endWrite()
}

func (d *DeviceOf[T]) fillScreen(c color.RGBA) {
	if d.rotation == NO_ROTATION || d.rotation == ROTATION_180 {
		d.fillRectangle(0, 0, d.width, d.height, c)
	} else {
		d.fillRectangle(0, 0, d.height, d.width, c)
	}
}

// Control the color format that is used when writing to the screen.
// The default is RGB565, setting it to any other value will break functions
// like SetPixel, FillRectangle, etc. Instead, you can write color data in the
// specified color format using DrawRGBBitmap8.
func (d *DeviceOf[T]) SetColorFormat(format ColorFormat) {
	d.startWrite()
	d.setColorFormat(format)
	d.endWrite()
}

func (d *DeviceOf[T]) setColorFormat(format ColorFormat) {
	// Lower 4 bits set the color format used in SPI.
	// Upper 4 bits set the color format used in the direct RGB interface.
	// The RGB interface is not currently supported, so it is left at a
	// reasonable default. Also, the RGB interface doesn't support RGB444.
	colmod := byte(format) | 0x50
	d.sendCommand(COLMOD, []byte{colmod})
}

// Rotation returns the current rotation of the device.
func (d *DeviceOf[T]) Rotation() Rotation {
	return d.rotation
}

// SetRotation changes the rotation of the device (clock-wise)
func (d *DeviceOf[T]) SetRotation(rotation Rotation) error {
	d.rotation = rotation
	d.startWrite()
	err := d.setRotation(rotation)
	d.endWrite()
	return err
}

func (d *DeviceOf[T]) setRotation(rotation Rotation) error {
	madctl := uint8(0)
	switch rotation % 4 {
	case NO_ROTATION:
		d.rowOffset = 0
		d.columnOffset = 0
	case ROTATION_90:
		madctl = MADCTL_MX | MADCTL_MV
		d.rowOffset = 0
		d.columnOffset = 0
	case ROTATION_180:
		madctl = MADCTL_MX | MADCTL_MY
		d.rowOffset = d.rowOffsetCfg
		d.columnOffset = d.columnOffsetCfg
	case ROTATION_270:
		madctl = MADCTL_MY | MADCTL_MV
		d.rowOffset = d.columnOffsetCfg
		d.columnOffset = d.rowOffsetCfg
	}
	if d.isBGR {
		madctl |= MADCTL_BGR
	}
	return d.sendCommand(MADCTL, []byte{madctl})
}

// Size returns the current size of the display.
func (d *DeviceOf[T]) Size() (w, h int16) {
	if d.rotation == NO_ROTATION || d.rotation == ROTATION_180 {
		return d.width, d.height
	}
	return d.height, d.width
}

// EnableBacklight enables or disables the backlight
func (d *DeviceOf[T]) EnableBacklight(enable bool) {
	d.backlight.Out(gpio.Level(enable))
}

// Set the sleep mode for this LCD panel. When sleeping, the panel uses a lot
// less power. The LCD won't display an image anymore, but the memory contents
// will be kept.
func (d *DeviceOf[T]) Sleep(sleepEnabled bool) error {
	if sleepEnabled {
		d.startWrite()
		d.sendCommand(SLPIN, nil)
		d.endWrite()
		time.Sleep(5 * time.Millisecond) // 5ms required by the datasheet
	} else {
		// Turn the LCD panel back on.
		d.startWrite()
		d.sendCommand(SLPOUT, nil)
		d.endWrite()
		// Note: the st7789 documentation says that it is needed to wait at
		// least 120ms before going to sleep again. Sleeping here would not be
		// practical (delays turning on the screen too much), so just hope the
		// screen won't need to sleep again for at least 120ms.
		// In practice, it's unlikely the user will set the display to sleep
		// again within 120ms.
	}
	return nil
}

// InvertColors inverts the colors of the screen
func (d *DeviceOf[T]) InvertColors(invert bool) {
	d.startWrite()
	if invert {
		d.sendCommand(INVON, nil)
	} else {
		d.sendCommand(INVOFF, nil)
	}
	d.endWrite()
}

// IsBGR changes the color mode (RGB/BGR)
func (d *DeviceOf[T]) IsBGR(bgr bool) {
	d.isBGR = bgr
}

// SetScrollArea sets an area to scroll with fixed top and bottom parts of the display.
func (d *DeviceOf[T]) SetScrollArea(topFixedArea, bottomFixedArea int16) {
	if d.height < 320 {
		// The screen doesn't use the full 320 pixel height.
		// Enlarge the bottom fixed area to fill the 320 pixel height, so that
		// bottomFixedArea starts from the visible bottom of the screen.
		topFixedArea += d.rowOffset
		bottomFixedArea += (320 - d.height) - d.rowOffset
	}
	if d.rotation == ROTATION_180 {
		// The screen is rotated by 180°, so we have to switch the top and
		// bottom fixed area.
		topFixedArea, bottomFixedArea = bottomFixedArea, topFixedArea
	}
	verticalScrollArea := 320 - topFixedArea - bottomFixedArea
	copy(d.buf[:6], []uint8{
		uint8(topFixedArea >> 8), uint8(topFixedArea),
		uint8(verticalScrollArea >> 8), uint8(verticalScrollArea),
		uint8(bottomFixedArea >> 8), uint8(bottomFixedArea)})
	d.startWrite()
	d.sendCommand(VSCRDEF, d.buf[:6])
	d.endWrite()
}

// SetScroll sets the vertical scroll address of the display.
func (d *DeviceOf[T]) SetScroll(line int16) {
	if d.rotation == ROTATION_180 {
		// The screen is rotated by 180°, so we have to invert the scroll line
		// (taking care of the RowOffset).
		line = (319 - d.rowOffset) - line
	}
	d.buf[0] = uint8(line >> 8)
	d.buf[1] = uint8(line)
	d.startWrite()
	d.sendCommand(VSCRSADD, d.buf[:2])
	d.endWrite()
}

// StopScroll returns the display to its normal state.
func (d *DeviceOf[T]) StopScroll() {
	d.startWrite()
	d.sendCommand(NORON, nil)
	d.endWrite()
}
