package diskqueue

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"
)

// logging stuff copied from github.com/nsqio/nsq/internal/lg

type LogLevel int

const (
	DEBUG = LogLevel(1)
	INFO  = LogLevel(2)
	WARN  = LogLevel(3)
	ERROR = LogLevel(4)
	FATAL = LogLevel(5)
)

type AppLogFunc func(lvl LogLevel, f string, args ...interface{})

func (l LogLevel) String() string {
	switch l {
	case 1:
		return "DEBUG"
	case 2:
		return "INFO"
	case 3:
		return "WARNING"
	case 4:
		return "ERROR"
	case 5:
		return "FATAL"
	}
	panic("invalid LogLevel")
}

type Interface interface {
	Put([]byte) error
	ReadChan() chan []byte // this is expected to be an *unbuffered* channel
	Close() error
	Delete() error
	Depth() int64
	Empty() error
	// fast forward to a new start point
	// parameters function returns 1 means forward, returns 0 means we may stop forward now.
	FastForward(func([]byte) int) error
	BufferPoolPut([]byte)
}

// diskQueue implements a filesystem backed FIFO queue
type diskQueue struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms

	// run-time state (also persisted to disk)
	readPos      int64
	writePos     int64
	readFileNum  int64
	writeFileNum int64
	depth        int64

	sync.RWMutex

	// instantiation time metadata
	name            string
	dataPath        string
	maxBytesPerFile int64 // currently this cannot change once created
	minMsgSize      int32
	maxMsgSize      int32
	syncEvery       int64         // number of writes per fsync
	syncTimeout     time.Duration // duration of time per fsync
	exitFlag        int32
	needSync        bool

	// keeps track of the position where we have read
	// (but not yet sent over readChan)
	nextReadPos     int64
	nextReadFileNum int64

	readFile  *os.File
	writeFile *os.File
	reader    *bufio.Reader
	writeBuf  bytes.Buffer

	// exposed via ReadChan()
	readChan chan []byte

	// internal channels
	writeChan               chan []byte
	writeResponseChan       chan error
	emptyChan               chan int
	emptyResponseChan       chan error
	exitChan                chan int
	exitSyncChan            chan int
	fastForwardChan         chan func([]byte) int
	fastForwardResponseChan chan error

	// reuse buf
	bufPool sync.Pool

	logf AppLogFunc
}

// New instantiates an instance of diskQueue, retrieving metadata
// from the filesystem and starting the read ahead goroutine
func New(name string, dataPath string, maxBytesPerFile int64,
	minMsgSize int32, maxMsgSize int32,
	syncEvery int64, syncTimeout time.Duration, logf AppLogFunc) Interface {
	d := diskQueue{
		name:                    name,
		dataPath:                dataPath,
		maxBytesPerFile:         maxBytesPerFile,
		minMsgSize:              minMsgSize,
		maxMsgSize:              maxMsgSize,
		readChan:                make(chan []byte),
		writeChan:               make(chan []byte),
		writeResponseChan:       make(chan error),
		emptyChan:               make(chan int),
		emptyResponseChan:       make(chan error),
		exitChan:                make(chan int),
		exitSyncChan:            make(chan int),
		fastForwardChan:         make(chan func([]byte) int),
		fastForwardResponseChan: make(chan error),
		syncEvery:               syncEvery,
		syncTimeout:             syncTimeout,
		logf:                    logf,
	}
	d.bufPool.New = func() interface{} {
		return make([]byte, maxMsgSize, maxMsgSize)
	}

	// no need to lock here, nothing else could possibly be touching this instance
	err := d.retrieveMetaData()
	if err != nil && !os.IsNotExist(err) {
		d.logf(ERROR, "DISKQUEUE(%s) failed to retrieveMetaData - %s", d.name, err)
	}

	go d.ioLoop()
	return &d
}

// Depth returns the depth of the queue
func (d *diskQueue) Depth() int64 {
	return atomic.LoadInt64(&d.depth)
}

// ReadChan returns the []byte channel for reading data
func (d *diskQueue) ReadChan() chan []byte {
	return d.readChan
}

// Put writes a []byte to the queue
func (d *diskQueue) Put(data []byte) error {
	d.RLock()
	defer d.RUnlock()

	if d.exitFlag == 1 {
		return errors.New("exiting")
	}

	d.writeChan <- data
	return <-d.writeResponseChan
}

// Close cleans up the queue and persists metadata
func (d *diskQueue) Close() error {
	err := d.exit(false)
	if err != nil {
		return err
	}
	return d.sync()
}

func (d *diskQueue) Delete() error {
	return d.exit(true)
}

func (d *diskQueue) exit(deleted bool) error {
	d.Lock()
	defer d.Unlock()

	d.exitFlag = 1

	if deleted {
		d.logf(INFO, "DISKQUEUE(%s): deleting", d.name)
	} else {
		d.logf(INFO, "DISKQUEUE(%s): closing", d.name)
	}

	close(d.exitChan)
	// ensure that ioLoop has exited
	<-d.exitSyncChan

	if d.readFile != nil {
		d.readFile.Close()
		d.readFile = nil
	}

	if d.writeFile != nil {
		d.writeFile.Close()
		d.writeFile = nil
	}

	return nil
}

// Empty destructively clears out any pending data in the queue
// by fast forwarding read positions and removing intermediate files
func (d *diskQueue) Empty() error {
	d.RLock()
	defer d.RUnlock()

	if d.exitFlag == 1 {
		return errors.New("exiting")
	}

	d.logf(INFO, "DISKQUEUE(%s): emptying", d.name)

	d.emptyChan <- 1
	return <-d.emptyResponseChan
}

func (d *diskQueue) deleteAllFiles() error {
	err := d.skipToNextRWFile()

	innerErr := os.Remove(d.metaDataFileName())
	if innerErr != nil && !os.IsNotExist(innerErr) {
		d.logf(ERROR, "DISKQUEUE(%s) failed to remove metadata file - %s", d.name, innerErr)
		return innerErr
	}

	return err
}

func (d *diskQueue) skipToNextRWFile() error {
	var err error

	if d.readFile != nil {
		d.readFile.Close()
		d.readFile = nil
	}

	if d.writeFile != nil {
		d.writeFile.Close()
		d.writeFile = nil
	}

	for i := d.readFileNum; i <= d.writeFileNum; i++ {
		fn := d.fileName(i)
		innerErr := os.Remove(fn)
		if innerErr != nil && !os.IsNotExist(innerErr) {
			d.logf(ERROR, "DISKQUEUE(%s) failed to remove data file - %s", d.name, innerErr)
			err = innerErr
		}
	}

	d.writeFileNum++
	d.writePos = 0
	d.readFileNum = d.writeFileNum
	d.readPos = 0
	d.nextReadFileNum = d.writeFileNum
	d.nextReadPos = 0
	atomic.StoreInt64(&d.depth, 0)

	return err
}

// readOne performs a low level filesystem read for a single []byte
// while advancing read positions and rolling files, if necessary
func (d *diskQueue) readOne() ([]byte, error) {
	var err error
	var msgSize int32

	if d.readFile == nil {
		curFileName := d.fileName(d.readFileNum)
		d.readFile, err = os.OpenFile(curFileName, os.O_RDONLY, 0600)
		if err != nil {
			return nil, err
		}

		d.logf(INFO, "DISKQUEUE(%s): readOne() opened %s", d.name, curFileName)

		if d.readPos > 0 {
			_, err = d.readFile.Seek(d.readPos, 0)
			if err != nil {
				d.readFile.Close()
				d.readFile = nil
				return nil, err
			}
		}

		d.reader = bufio.NewReader(d.readFile)
	}

	err = binary.Read(d.reader, binary.BigEndian, &msgSize)
	if err != nil {
		d.readFile.Close()
		d.readFile = nil
		return nil, err
	}

	if msgSize < d.minMsgSize || msgSize > d.maxMsgSize {
		// this file is corrupt and we have no reasonable guarantee on
		// where a new message should begin
		d.readFile.Close()
		d.readFile = nil
		return nil, fmt.Errorf("invalid message read size (%d)", msgSize)
	}

	buf := d.BufferPoolGet()
	readBuf := buf[:msgSize] // make([]byte, msgSize)
	_, err = io.ReadFull(d.reader, readBuf)
	if err != nil {
		d.readFile.Close()
		d.readFile = nil
		return nil, err
	}

	totalBytes := int64(4 + msgSize)

	// we only advance next* because we have not yet sent this to consumers
	// (where readFileNum, readPos will actually be advanced)
	d.nextReadPos = d.readPos + totalBytes
	d.nextReadFileNum = d.readFileNum

	// TODO: each data file should embed the maxBytesPerFile
	// as the first 8 bytes (at creation time) ensuring that
	// the value can change without affecting runtime
	if d.nextReadPos > d.maxBytesPerFile {
		if d.readFile != nil {
			d.readFile.Close()
			d.readFile = nil
		}

		d.nextReadFileNum++
		d.nextReadPos = 0
	}

	return readBuf, nil
}

// writeOne performs a low level filesystem write for a single []byte
// while advancing write positions and rolling files, if necessary
func (d *diskQueue) writeOne(data []byte) error {
	var err error

	if d.writeFile == nil {
		curFileName := d.fileName(d.writeFileNum)
		d.writeFile, err = os.OpenFile(curFileName, os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			return err
		}

		d.logf(INFO, "DISKQUEUE(%s): writeOne() opened %s", d.name, curFileName)

		if d.writePos > 0 {
			_, err = d.writeFile.Seek(d.writePos, 0)
			if err != nil {
				d.writeFile.Close()
				d.writeFile = nil
				return err
			}
		}
	}

	dataLen := int32(len(data))

	if dataLen < d.minMsgSize || dataLen > d.maxMsgSize {
		return fmt.Errorf("invalid message write size (%d) maxMsgSize=%d", dataLen, d.maxMsgSize)
	}

	d.writeBuf.Reset()
	err = binary.Write(&d.writeBuf, binary.BigEndian, dataLen)
	if err != nil {
		return err
	}

	_, err = d.writeBuf.Write(data)
	if err != nil {
		return err
	}

	// only write to the file once
	_, err = d.writeFile.Write(d.writeBuf.Bytes())
	if err != nil {
		d.writeFile.Close()
		d.writeFile = nil
		return err
	}

	totalBytes := int64(4 + dataLen)
	d.writePos += totalBytes
	atomic.AddInt64(&d.depth, 1)

	if d.writePos > d.maxBytesPerFile {
		d.writeFileNum++
		d.writePos = 0

		// sync every time we start writing to a new file
		err = d.sync()
		if err != nil {
			d.logf(ERROR, "DISKQUEUE(%s) failed to sync - %s", d.name, err)
		}

		if d.writeFile != nil {
			d.writeFile.Close()
			d.writeFile = nil
		}
	}

	return err
}

// sync fsyncs the current writeFile and persists metadata
func (d *diskQueue) sync() error {
	if d.writeFile != nil {
		err := d.writeFile.Sync()
		if err != nil {
			d.writeFile.Close()
			d.writeFile = nil
			return err
		}
	}

	err := d.persistMetaData()
	if err != nil {
		return err
	}

	d.needSync = false
	return nil
}

// retrieveMetaData initializes state from the filesystem
func (d *diskQueue) retrieveMetaData() error {
	var f *os.File
	var err error

	fileName := d.metaDataFileName()
	f, err = os.OpenFile(fileName, os.O_RDONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	var depth int64
	_, err = fmt.Fscanf(f, "%d\n%d,%d\n%d,%d\n",
		&depth,
		&d.readFileNum, &d.readPos,
		&d.writeFileNum, &d.writePos)
	if err != nil {
		return err
	}
	atomic.StoreInt64(&d.depth, depth)
	d.nextReadFileNum = d.readFileNum
	d.nextReadPos = d.readPos

	return nil
}

// persistMetaData atomically writes state to the filesystem
func (d *diskQueue) persistMetaData() error {
	var f *os.File
	var err error

	fileName := d.metaDataFileName()
	tmpFileName := fmt.Sprintf("%s.%d.tmp", fileName, rand.Int())

	// write to tmp file
	f, err = os.OpenFile(tmpFileName, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(f, "%d\n%d,%d\n%d,%d\n",
		atomic.LoadInt64(&d.depth),
		d.readFileNum, d.readPos,
		d.writeFileNum, d.writePos)
	if err != nil {
		f.Close()
		return err
	}
	f.Sync()
	f.Close()

	// atomically rename
	return os.Rename(tmpFileName, fileName)
}

func (d *diskQueue) metaDataFileName() string {
	return fmt.Sprintf(path.Join(d.dataPath, "%s.diskqueue.meta.dat"), d.name)
}

func (d *diskQueue) fileName(fileNum int64) string {
	return fmt.Sprintf(path.Join(d.dataPath, "%s.diskqueue.%06d.dat"), d.name, fileNum)
}

func (d *diskQueue) checkTailCorruption(depth int64) {
	if d.readFileNum < d.writeFileNum || d.readPos < d.writePos {
		return
	}

	// we've reached the end of the diskqueue
	// if depth isn't 0 something went wrong
	if depth != 0 {
		if depth < 0 {
			d.logf(ERROR,
				"DISKQUEUE(%s) negative depth at tail (%d), metadata corruption, resetting 0...",
				d.name, depth)
		} else if depth > 0 {
			d.logf(ERROR,
				"DISKQUEUE(%s) positive depth at tail (%d), data loss, resetting 0...",
				d.name, depth)
		}
		// force set depth 0
		atomic.StoreInt64(&d.depth, 0)
		d.needSync = true
	}

	if d.readFileNum != d.writeFileNum || d.readPos != d.writePos {
		if d.readFileNum > d.writeFileNum {
			d.logf(ERROR,
				"DISKQUEUE(%s) readFileNum > writeFileNum (%d > %d), corruption, skipping to next writeFileNum and resetting 0...",
				d.name, d.readFileNum, d.writeFileNum)
		}

		if d.readPos > d.writePos {
			d.logf(ERROR,
				"DISKQUEUE(%s) readPos > writePos (%d > %d), corruption, skipping to next writeFileNum and resetting 0...",
				d.name, d.readPos, d.writePos)
		}

		d.skipToNextRWFile()
		d.needSync = true
	}
}

func (d *diskQueue) moveForward() {
	oldReadFileNum := d.readFileNum
	d.readFileNum = d.nextReadFileNum
	d.readPos = d.nextReadPos
	depth := atomic.AddInt64(&d.depth, -1)

	// see if we need to clean up the old file
	if oldReadFileNum != d.nextReadFileNum {
		// sync every time we start reading from a new file
		d.needSync = true

		fn := d.fileName(oldReadFileNum)
		err := os.Remove(fn)
		if err != nil {
			d.logf(ERROR, "DISKQUEUE(%s) failed to Remove(%s) - %s", d.name, fn, err)
		}
	}

	d.checkTailCorruption(depth)
}

func (d *diskQueue) handleReadError() {
	// jump to the next read file and rename the current (bad) file
	if d.readFileNum == d.writeFileNum {
		// if you can't properly read from the current write file it's safe to
		// assume that something is fucked and we should skip the current file too
		if d.writeFile != nil {
			d.writeFile.Close()
			d.writeFile = nil
		}
		d.writeFileNum++
		d.writePos = 0
	}

	badFn := d.fileName(d.readFileNum)
	badRenameFn := badFn + ".bad"

	d.logf(WARN,
		"DISKQUEUE(%s) jump to next file and saving bad file as %s",
		d.name, badRenameFn)

	err := os.Rename(badFn, badRenameFn)
	if err != nil {
		d.logf(ERROR,
			"DISKQUEUE(%s) failed to rename bad diskqueue file %s to %s",
			d.name, badFn, badRenameFn)
	}

	d.readFileNum++
	d.readPos = 0
	d.nextReadFileNum = d.readFileNum
	d.nextReadPos = 0

	// significant state change, schedule a sync on the next iteration
	d.needSync = true
}

// ioLoop provides the backend for exposing a go channel (via ReadChan())
// in support of multiple concurrent queue consumers
//
// it works by looping and branching based on whether or not the queue has data
// to read and blocking until data is either read or written over the appropriate
// go channels
//
// conveniently this also means that we're asynchronously reading from the filesystem
func (d *diskQueue) ioLoop() {
	var dataRead []byte
	var err error
	var count int64
	var r chan []byte

	syncTicker := time.NewTicker(d.syncTimeout)

	for {
		// dont sync all the time :)
		if count == d.syncEvery {
			d.needSync = true
		}

		if d.needSync {
			err = d.sync()
			if err != nil {
				d.logf(ERROR, "DISKQUEUE(%s) failed to sync - %s", d.name, err)
			}
			count = 0
		}

		if (d.readFileNum < d.writeFileNum) || (d.readPos < d.writePos) {
			if d.nextReadPos == d.readPos {
				dataRead, err = d.readOne()
				if err != nil {
					d.logf(ERROR, "DISKQUEUE(%s) reading at %d of %s - %s",
						d.name, d.readPos, d.fileName(d.readFileNum), err)
					d.handleReadError()
					continue
				}
			}
			r = d.readChan
		} else {
			r = nil
		}

		select {
		// the Go channel spec dictates that nil channel operations (read or write)
		// in a select are skipped, we set r to d.readChan only when there is data to read
		case r <- dataRead:
			count++
			// moveForward sets needSync flag if a file is removed
			d.moveForward()
		case <-d.emptyChan:
			d.emptyResponseChan <- d.deleteAllFiles()
			count = 0
		case dataWrite := <-d.writeChan:
			count++
			d.writeResponseChan <- d.writeOne(dataWrite)
		case f := <-d.fastForwardChan:
			d.fastForwardResponseChan <- d.fastForward(dataRead, f)
		case <-syncTicker.C:
			if count == 0 {
				// avoid sync when there's no activity
				continue
			}
			d.needSync = true
		case <-d.exitChan:
			goto exit
		}
	}

exit:
	d.logf(INFO, "DISKQUEUE(%s): closing ... ioLoop", d.name)
	syncTicker.Stop()
	d.exitSyncChan <- 1
}

func (d *diskQueue) FastForward(f func([]byte) int) error {
	// from readPos to writePos
	// we can try to skip half of all files at one time.
	d.RLock()
	defer d.RUnlock()

	if d.exitFlag == 1 {
		return errors.New("exiting")
	}

	d.fastForwardChan <- f
	return <-d.fastForwardResponseChan
}

func (d *diskQueue) BufferPoolGet() []byte {
	return d.bufPool.Get().([]byte)
}

func (d *diskQueue) BufferPoolPut(b []byte) {
	if cap(b) != int(d.maxMsgSize) {
		return
	}
	b = b[:cap(b)]
	d.bufPool.Put(b)
}

func (d *diskQueue) fastForward(dataRead []byte, f func([]byte) int) error {
	var err error
	oldDataRead := dataRead

	// data is current data and ready to send over the channel
	if (d.readFileNum < d.writeFileNum) || (d.readPos < d.writePos) {
		// start from d.nextReadFileNum, d.nextReadPos, dataRead
		lastStopReadFileNum, lastStopReadPos := d.readFileNum, d.readPos
		currReadFileNum, currReadPos := lastStopReadFileNum, lastStopReadPos
		beginReadFileNum, beginReadPos := lastStopReadFileNum, lastStopReadPos
		endWriteFileNum, endWritePos := d.writeFileNum, d.writePos
		// get one buf from pool
		buf := d.BufferPoolGet()
		defer d.BufferPoolPut(buf)
		if d.nextReadPos == d.readPos {
			// start to peek next data, if it isn't stop, we have to find a stop signal
			dataRead, err = d.peekOne(nil, nil, buf, lastStopReadFileNum, lastStopReadPos)
			if err != nil {
				return err
			}
		}
		for {
			if len(dataRead) == 0 {
				// continue to seek
				break
			}
			// stop forward
			if f(dataRead) == 0 {
				endWriteFileNum, endWritePos = currReadFileNum, currReadPos
				lastStopReadFileNum, lastStopReadPos = currReadFileNum, currReadPos
				// do we have data to backward half?
				if beginReadFileNum < currReadFileNum {
					// backward half
					currReadFileNum = beginReadFileNum + (currReadFileNum-beginReadFileNum)/2
					if currReadFileNum == beginReadFileNum {
						currReadPos = beginReadPos
						// search this file to the end
						lastStopReadPos = d.fastForwardInFile(f, buf, currReadFileNum, currReadPos)
						currReadPos = lastStopReadPos
						break
					} else {
						currReadPos = 0
						// search first message
						dataRead, err = d.peekOne(nil, nil, buf, currReadFileNum, currReadPos)
						if err != nil {
							break
						}
					}
				} else if beginReadFileNum == currReadFileNum {
					// this is the end
					break
				} else {
					// this is the end
					break
				}
			} else {
				// continue forward
				beginReadFileNum, beginReadPos = currReadFileNum, currReadPos
				// do we have data to forward half?
				// reach the end?
				if currReadFileNum < endWriteFileNum {
					// forward half
					currReadFileNum = currReadFileNum + (endWriteFileNum-currReadFileNum+1)/2
					currReadPos = 0
					dataRead, err = d.peekOne(nil, nil, buf, currReadFileNum, currReadPos)
					if err != nil {
						break
					}
				} else if currReadFileNum == endWriteFileNum {
					if currReadPos < endWritePos {
						lastStopReadFileNum = currReadFileNum
						// search from currReadPos to endWritePos or the end of file
						lastStopReadPos = d.fastForwardInFile(f, buf, currReadFileNum, currReadPos)
						currReadPos = lastStopReadPos
						break
					} else {
						// this is the end
						break
					}
				} else {
					// this is the end
					break
				}
			}
		}
		// eventually we need to set readFileNum and readPos to a new position.
		if d.readFileNum != lastStopReadFileNum || d.readPos != lastStopReadPos {
			// reclaim oldDataRead
			d.BufferPoolPut(oldDataRead)
			if d.readFileNum != lastStopReadFileNum {
				// TODO: remove all file from d.readFileNum to lastStopReadFileNum
				if d.readFile != nil {
					d.readFile.Close()
					d.readFile = nil
				}

				if d.writeFile != nil {
					d.sync()
				}

				d.removeFiles(d.readFileNum, lastStopReadFileNum)
				// recalculate the depth
				depth := d.depthInFiles(lastStopReadFileNum, lastStopReadPos, d.writeFileNum, d.writePos)
				atomic.StoreInt64(&d.depth, depth)
			}
			d.readFileNum, d.readPos = lastStopReadFileNum, lastStopReadPos
			d.nextReadFileNum, d.nextReadPos = d.readFileNum, d.readPos
		} else {
			// we didn't move our position
		}
		return err
	} else {
		// data is invalid
		return err
	}
}

func (d *diskQueue) removeFiles(readFileNum, endFileNum int64) (err error) {
	for i := readFileNum; i < endFileNum; i++ {
		fn := d.fileName(i)
		innerErr := os.Remove(fn)
		if innerErr != nil && !os.IsNotExist(innerErr) {
			d.logf(ERROR, "DISKQUEUE(%s) failed to remove data file - %s", d.name, innerErr)
			err = innerErr
		}
	}
	return
}

func (d *diskQueue) depthInFiles(readFileNum, readPos, endFileNum, endReadPos int64) (depth int64) {
	for i := readFileNum; i <= endFileNum; i++ {
		endPos := endReadPos
		if i != endFileNum {
			endPos = -1
		}
		depth += d.depthInFile(i, readPos, endPos)
	}
	return
}

func (d *diskQueue) depthInFile(readFileNum, readPos, endPos int64) (depth int64) {
	var msgSize int32
	if -1 != endPos && endPos-readPos < 4 {
		return
	}

	readFile, reader, err := d.openFile(readFileNum, readPos)
	if err != nil {
		return
	}
	defer readFile.Close()
	for {
		err = binary.Read(reader, binary.BigEndian, &msgSize)
		if err != nil {
			return
		}
		readPos += 4
		if -1 != endPos && endPos-readPos < int64(msgSize) {
			return
		}

		if msgSize < d.minMsgSize || msgSize > d.maxMsgSize {
			// this file is corrupt and we have no reasonable guarantee on
			// where a new message should begin
			return
		}
		discarded, err := reader.Discard(int(msgSize))
		if err != nil || discarded != int(msgSize) {
			return
		}
		depth++
		readPos += int64(msgSize)
		if -1 != endPos && endPos-readPos < 4 {
			return
		}
	}
}

func (d *diskQueue) fastForwardInFile(f func([]byte) int, buf []byte, readFileNum, readPos int64) (lastStopReadPos int64) {
	currReadPos := readPos
	currReadFileNum := readFileNum
	readFile, reader, err := d.openFile(currReadFileNum, currReadPos)
	if err != nil {
		return
	}
	defer readFile.Close()
	for {
		dataRead, err := d.peekOne(readFile, reader, buf, currReadFileNum, currReadPos)
		if err != nil || len(dataRead) == 0 {
			return
		}
		if f(dataRead) == 0 {
			return
		} else {
			currReadPos += int64(4 + len(dataRead))
			lastStopReadPos = currReadPos
		}
	}
}

func (d *diskQueue) openFile(readFileNum, readPos int64) (readFile *os.File, reader *bufio.Reader, err error) {
	curFileName := d.fileName(readFileNum)
	readFile, err = os.OpenFile(curFileName, os.O_RDONLY, 0600)
	if err != nil {
		readFile = nil
		return readFile, reader, err
	}

	d.logf(INFO, "DISKQUEUE(%s): readOne() opened %s", d.name, curFileName)

	if readPos > 0 {
		_, err = readFile.Seek(readPos, 0)
		if err != nil {
			readFile.Close()
			readFile = nil
			return readFile, reader, err
		}
	}

	reader = bufio.NewReader(readFile)
	return readFile, reader, err
}

func (d *diskQueue) peekOne(readFile *os.File, reader *bufio.Reader, buf []byte, readFileNum, readPos int64) ([]byte, error) {
	var err error
	var msgSize int32

	if readFile == nil {
		readFile, reader, err = d.openFile(readFileNum, readPos)
		if err != nil {
			return nil, err
		}
		defer readFile.Close()
	}

	err = binary.Read(reader, binary.BigEndian, &msgSize)
	if err != nil {
		return nil, err
	}

	if msgSize < d.minMsgSize || msgSize > d.maxMsgSize {
		// this file is corrupt and we have no reasonable guarantee on
		// where a new message should begin
		return nil, fmt.Errorf("invalid message read size (%d)", msgSize)
	}

	readBuf := buf[:msgSize] //make([]byte, msgSize)
	_, err = io.ReadFull(reader, readBuf)
	if err != nil {
		return nil, err
	}

	return readBuf, err
}
