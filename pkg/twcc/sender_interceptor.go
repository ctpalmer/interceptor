package twcc

import (
	"math/rand"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtp/v2"
)

// SenderInterceptor sends transport wide congestion control reports as specified in:
// https://datatracker.ietf.org/doc/html/draft-holmer-rmcat-transport-wide-cc-extensions-01
type SenderInterceptor struct {
	interceptor.NoOp

	log logging.LeveledLogger

	m     sync.Mutex
	wg    sync.WaitGroup
	close chan struct{}

	interval time.Duration

	recorder   *Recorder
	packetChan chan packet
}

// An Option is a function that can be used to configure a SenderInterceptor
type Option func(*SenderInterceptor) error

// SendInterval sets the interval at which the interceptor
// will send new feedback reports.
func SendInterval(interval time.Duration) Option {
	return func(s *SenderInterceptor) error {
		s.interval = interval
		return nil
	}
}

// NewSenderInterceptor returns a new SenderInterceptor configured with the given options.
func NewSenderInterceptor(opts ...Option) (*SenderInterceptor, error) {
	i := &SenderInterceptor{
		log:        logging.NewDefaultLoggerFactory().NewLogger("twcc_sender_interceptor"),
		packetChan: make(chan packet),
		close:      make(chan struct{}),
		interval:   100 * time.Millisecond,
	}

	for _, opt := range opts {
		err := opt(i)
		if err != nil {
			return nil, err
		}
	}

	return i, nil
}

// BindRTCPWriter lets you modify any outgoing RTCP packets. It is called once per PeerConnection. The returned method
// will be called once per packet batch.
func (s *SenderInterceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	s.m.Lock()
	defer s.m.Unlock()

	s.recorder = NewRecorder(rand.Uint32()) // #nosec

	if s.isClosed() {
		return writer
	}

	s.wg.Add(1)

	go s.loop(writer)

	return writer
}

type packet struct {
	hdr   *rtp.Header
	seqNr uint16
	ts    int64
	ssrc  uint32
}

// BindRemoteStream lets you modify any incoming RTP packets. It is called once for per RemoteStream. The returned method
// will be called once per rtp packet.
func (s *SenderInterceptor) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	var hdrExtID uint8
	for _, e := range info.RTPHeaderExtensions {
		if e.URI == transportCCURI {
			hdrExtID = uint8(e.ID)
			break
		}
	}
	if hdrExtID == 0 { // Don't try to read header extension if ID is 0, because 0 is an invalid extension ID
		return reader
	}
	return interceptor.RTPReaderFunc(func(buf []byte, attributes interceptor.Attributes) (int, interceptor.Attributes, error) {
		i, attr, err := reader.Read(buf, attributes)
		if err != nil {
			return 0, nil, err
		}
		p := rtp.Packet{}
		err = p.Unmarshal(buf[:i])
		if err != nil {
			return 0, nil, err
		}
		var tccExt rtp.TransportCCExtension
		if ext := p.GetExtension(hdrExtID); ext != nil {
			err = tccExt.Unmarshal(ext)
			if err != nil {
				return 0, nil, err
			}

			s.packetChan <- packet{
				hdr:   &p.Header,
				seqNr: tccExt.TransportSequence,
				ts:    time.Now().UnixNano(),
				ssrc:  info.SSRC,
			}
		}

		return i, attr, nil
	})
}

// Close closes the interceptor.
func (s *SenderInterceptor) Close() error {
	defer s.wg.Wait()
	s.m.Lock()
	defer s.m.Unlock()

	if !s.isClosed() {
		close(s.close)
	}

	return nil
}

func (s *SenderInterceptor) isClosed() bool {
	select {
	case <-s.close:
		return true
	default:
		return false
	}
}

func (s *SenderInterceptor) loop(w interceptor.RTCPWriter) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.interval)

	for {
		select {
		case <-s.close:
			return
		case p := <-s.packetChan:
			s.recorder.Record(p.ssrc, p.seqNr, p.ts/1e6) // ns -> ms: divide by 1e6

		case <-ticker.C:
			// build and send twcc
			if s.recorder != nil {
				pkts := s.recorder.BuildFeedbackPacket()
				_, err := w.Write(pkts, nil)
				if err != nil {
					s.log.Error(err.Error())
				}
			}
		}
	}
}