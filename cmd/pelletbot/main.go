package main

import (
	_ "embed"

	"fmt"
	"image"
	"log"
	"time"

	"github.com/AlCutter/pelletbot/internal/st7789"
	"github.com/AlCutter/pelletbot/internal/st7789/pixel"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/i2c/i2creg"
	"periph.io/x/conn/v3/physic"
	"periph.io/x/conn/v3/spi/spireg"
	"periph.io/x/host/v3"

	"gocv.io/x/gocv"
)

var (
	// go:embed test.jpg
	J []byte
)

func init() {
	if _, err := host.Init(); err != nil {
		log.Fatal(err)
	}
}

func main() {
	spiPort, err := spireg.Open("SPI0.1")
	if err != nil {
		panic(err)
	}
	defer spiPort.Close()
	// Use i2creg I²C bus registry to find the first available I²C bus.
	i2cBus, err := i2creg.Open("I2C1")
	if err != nil {
		log.Fatal(err)
	}
	defer i2cBus.Close()
	i2cBus.SetSpeed(physic.Frequency(60 * 1000 * 1000))
	dsp, err := st7789.New(spiPort, gpioreg.ByName("GPIO9"))
	if err != nil {
		panic(err)
	}
	dsp.Configure(st7789.Config{
		Width:     240,
		Height:    320,
		Rotation:  st7789.ROTATION_180,
		RowOffset: 00,
		FrameRate: st7789.FRAMERATE_60,

		//VSyncLines: st7789.MAX_VSYNC_SCANLINES,
	})
	dsp.IsBGR(true)
	dsp.InvertColors(true)
	/*
		pwm, err := pca9685.NewI2C(i2cBus, pca9685.I2CAddr)
		if err != nil {
			panic(err)
		}
		servoGroup := pca9685.NewServoGroup(pwm, 50, 650, 0, 180)
	*/
	camID := 0
	webcam, err := gocv.OpenVideoCapture(camID)
	if err != nil {
		fmt.Printf("Error opening video capture device: %v\n", camID)
		return
	}
	fmt.Printf("is opened: %v\n", webcam.IsOpened())
	fmt.Printf("codec: %v\n", webcam.CodecString())

	defer webcam.Close()
	webcam.Set(gocv.VideoCaptureFrameWidth, 320)
	webcam.Set(gocv.VideoCaptureFrameHeight, 240)
	webcam.Set(gocv.VideoCaptureConvertRGB, 1)
	// streaming, capture from webcam
	buf := gocv.NewMat()
	defer buf.Close()

	fmt.Printf("Start reading device: %v\n", camID)
	for i := 0; i < 100; i++ {
		fmt.Printf("frame %d ", i)
		webcam.Grab(1)
		if ok := webcam.Retrieve(&buf); !ok {
			fmt.Printf("Device %v closed\n", camID)
			return
		}
		if buf.Empty() {
			continue
		}
		gocv.Resize(buf, &buf, image.Point{X: 240, Y: 320}, 0, 0, gocv.InterpolationDefault)
		fmt.Printf("Mat size: %dx%d type %d\n", buf.Cols(), buf.Rows(), buf.Type())
		gocv.CvtColor(buf, &buf, gocv.ColorRGBAToBGR565)

		i := pixel.NewImage[pixel.RGB565BE](buf.Cols(), buf.Rows())
		d, err := buf.DataPtrUint8()
		if err != nil {
			fmt.Println(err)
			continue
		}
		copy(i.RawBuffer(), d)
		if err := dsp.DrawBitmap(0, 0, i); err != nil {
			fmt.Println(err)
		}

		//dsp.FillRectangle(10, 10, 200, 200, color.RGBA{R: 0xff})
		time.Sleep(100 * time.Millisecond)

	}
	fmt.Println("Done.")

	/*
		for {
				a := physic.Angle(rand.Intn(180) * 0)
				if err := servoGroup.GetServo(0).SetAngle(a); err != nil {
					panic(err)
				}
			// Set the screen color to white
			time.Sleep(time.Second)
		}
	*/

	dsp.EnableBacklight(false)

}
