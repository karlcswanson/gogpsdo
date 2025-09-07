package main

import (
    "encoding/binary"
    "bytes"
    "fmt"
    "log"
    "net"
    "os"
    "os/signal"
    "sync"
    "syscall"
    "time"
    "flag"

    "github.com/tarm/serial"
    "golang.org/x/sys/unix"
)

// GPSDOStatus represents the GPSDO operational state
type GPSDOStatus int

const (
    GPSDOPowerUp GPSDOStatus = iota
    GPSDOHoldover
    GPSDOLocked
    GPSDOUnknown
)

func (s GPSDOStatus) String() string {
    switch s {
    case GPSDOPowerUp:
        return "POWER_UP"
    case GPSDOHoldover:
        return "HOLDOVER"
    case GPSDOLocked:
        return "LOCKED"
    default:
        return "UNKNOWN"
    }
}

// Z3805AData represents parsed data from HP Z3805A GPSDO
type Z3805AData struct {
    Year        int
    DayOfYear   int
    Hour        int
    Minute      int
    Second      int
    LeapSeconds int
    Status      GPSDOStatus
    Valid       bool
    Timestamp   time.Time
    ParseTime   time.Time
}

// GPSDOChronySock manages the GPSDO to Chrony SOCK interface
type GPSDOChronySock struct {
    serialPort string
    sockPath   string
    mutex      sync.RWMutex
    stats      struct {
        totalPackets  uint64
        validPackets  uint64
        chronySamples uint64
        lastUpdate    time.Time
    }
}

func NewGPSDOChronySock(serialPort, sockPath string) *GPSDOChronySock {
    return &GPSDOChronySock{
        serialPort: serialPort,
        sockPath:   sockPath,
    }
}

func (g *GPSDOChronySock) parseZ3805APacket(data []byte) *Z3805AData {
    if len(data) != 16 || data[15] != 0x0D {
        return nil
    }

    // Extract BCD values exactly as documented
    year := 2000 + int(data[0])*10 + int(data[1])
    dayOfYear := int(data[2])*100 + int(data[3])*10 + int(data[4])
    hour := int(data[5])*10 + int(data[6])
    minute := int(data[7])*10 + int(data[8])
    second := int(data[9])*10 + int(data[10])
    leapSeconds := int(data[11])*10 + int(data[12])
    statusVal := int(data[13])*10 + int(data[14])

    // Validate ranges
    if year < 2000 || year > 2099 || dayOfYear < 1 || dayOfYear > 366 ||
        hour > 23 || minute > 59 || second > 59 {
        return nil
    }

    // Convert status to enum per Z3805A documentation
    var status GPSDOStatus
    switch statusVal {
    case 0:
        status = GPSDOLocked // GPS Lock Mode
    case 10:
        status = GPSDOPowerUp // Power-Up Mode
    case 100:
        status = GPSDOHoldover // Holdover Mode
    default:
        status = GPSDOUnknown
    }

    // Convert day of year to proper date
    startOfYear := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
    targetDate := startOfYear.AddDate(0, 0, dayOfYear-1)
    timestamp := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(),
        hour, minute, second, 0, time.UTC)


    // --- GPS week rollover fix ---
    // If the year is suspiciously old, add 1024 weeks (7168 days)
    if year < 2020 {
        timestamp = timestamp.AddDate(0, 0, 7168)
        year = timestamp.Year()
        dayOfYear = timestamp.YearDay()
    }


    return &Z3805AData{
        Year:        year,
        DayOfYear:   dayOfYear,
        Hour:        hour,
        Minute:      minute,
        Second:      second,
        LeapSeconds: leapSeconds,
        Status:      status,
        Valid:       status == GPSDOLocked || status == GPSDOHoldover,
        Timestamp:   timestamp,
        ParseTime:   time.Now(),
    }
}


type sockSample struct {
    Tv     unix.Timeval
    Offset float64
    Pulse  int32
    Leap   int32
    Pad    int32
    Magic  int32
}

func (g *GPSDOChronySock) sendChronySample(data *Z3805AData) {
    if data == nil || !data.Valid {
        return
    }

    sample := sockSample{
        Tv: unix.Timeval{
            Sec: data.Timestamp.Unix(),
            Usec: int64(data.Timestamp.Nanosecond() / 1000),
        },
        Offset: 0,
        Pulse:  0,
        Leap:   0,
        Pad:    0,
        Magic:  0x534f434b,
    }

    buf := new(bytes.Buffer)
    // Use native endianness (little-endian on ARM)
    if err := binary.Write(buf, binary.LittleEndian, sample); err != nil {
        log.Printf("Failed to encode sample: %v", err)
        return
    }

    conn, err := net.Dial("unixgram", g.sockPath)
    if err != nil {
        log.Printf("Failed to connect to chrony socket: %v", err)
        return
    }
    defer conn.Close()

    conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
    if _, err := conn.Write(buf.Bytes()); err != nil {
        log.Printf("Failed to write to chrony socket: %v", err)
        return
    }

    g.mutex.Lock()
    g.stats.chronySamples++
    g.mutex.Unlock()
    log.Printf("Chrony binary sample sent: GPS=%04d-%03d %02d:%02d:%02d UTC, Status=%s, Leap=%d",
        data.Year, data.DayOfYear, data.Hour, data.Minute, data.Second,
        data.Status.String(), data.LeapSeconds)
}

func (g *GPSDOChronySock) Run() error {
    log.Printf("Starting GPSDO-Chrony SOCK bridge")
    log.Printf("Serial: %s, Socket: %s", g.serialPort, g.sockPath)

    // Open serial port
    config := &serial.Config{
        Name:        g.serialPort,
        Baud:        9600,
        Size:        8,
        Parity:      serial.ParityNone,
        StopBits:    serial.Stop1,
        ReadTimeout: time.Second,
    }

    port, err := serial.OpenPort(config)
    if err != nil {
        return fmt.Errorf("failed to open serial port: %w", err)
    }
    defer port.Close()

    log.Printf("Serial port opened successfully")

    // Setup signal handling
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

    var wg sync.WaitGroup
    var currentData *Z3805AData
    var dataMutex sync.RWMutex

    // Use a done channel to coordinate shutdown
    done := make(chan struct{})

    // Status reporting goroutine
    wg.Add(1)
    go func() {
        defer wg.Done()
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()

        for {
            select {
            case <-done:
                return
            case <-ticker.C:
                g.mutex.RLock()
                stats := g.stats
                g.mutex.RUnlock()

                dataMutex.RLock()
                data := currentData
                dataMutex.RUnlock()

                log.Printf("=== GPSDO Status ===")
                log.Printf("Packets: Total=%d, Valid=%d", stats.totalPackets, stats.validPackets)
                log.Printf("Chrony: Samples=%d", stats.chronySamples)

                if data != nil {
                    age := time.Since(stats.lastUpdate)
                    log.Printf("Current: %s UTC, Status=%s, Age=%s",
                        data.Timestamp.Format("15:04:05"), data.Status.String(), age.Truncate(time.Second))
                }
                log.Printf("==================")
            }
        }
    }()

    // Serial reader main loop
    buffer := make([]byte, 16)

    run := true
    go func() {
        <-sigChan
        log.Println("Shutdown signal received")
        run = false
        close(done)
    }()

    for run {
        n, err := port.Read(buffer)
        if err != nil {
            continue // Timeout is normal - Z3805A sends every 2 seconds
        }

        if n == 16 {
            g.mutex.Lock()
            g.stats.totalPackets++
            g.mutex.Unlock()

            if data := g.parseZ3805APacket(buffer); data != nil {
                g.mutex.Lock()
                g.stats.validPackets++
                g.stats.lastUpdate = time.Now()
                g.mutex.Unlock()

                dataMutex.Lock()
                currentData = data
                dataMutex.Unlock()

                log.Printf("GPSDO: %04d-%03d %02d:%02d:%02d UTC, Status=%s, Leap=%d",
                    data.Year, data.DayOfYear, data.Hour, data.Minute, data.Second,
                    data.Status.String(), data.LeapSeconds)

                // Send to chrony
                g.sendChronySample(data)
            }
        }
    }

    wg.Wait()
    return nil
}

func main() {
    serialPort := flag.String("port", "/dev/ttyAMA0", "TOD TTY Input")
    sockPath := flag.String("sock", "/var/run/chrony/gpsdo.sock", "Chrony SOCK refclock path")
    flag.Parse() 

    // Check if serial port exists
    if _, err := os.Stat(*serialPort); os.IsNotExist(err) {
        log.Fatalf("Serial port %s does not exist", *serialPort)
    }

    bridge := NewGPSDOChronySock(*serialPort, *sockPath)

    if err := bridge.Run(); err != nil {
        log.Fatalf("Bridge error: %v", err)
    }
}