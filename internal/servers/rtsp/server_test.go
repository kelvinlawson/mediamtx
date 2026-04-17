package rtsp

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	rtspauth "github.com/bluenviron/gortsplib/v5/pkg/auth"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/sdp"
	mpegts "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/test"
	"github.com/bluenviron/mediamtx/internal/unit"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func ptrOf[T any](v T) *T {
	p := new(T)
	*p = v
	return p
}

type dummyPath struct{}

func (p *dummyPath) Name() string {
	return "teststream"
}

func (p *dummyPath) SafeConf() *conf.Path {
	return &conf.Path{}
}

func (p *dummyPath) ExternalCmdEnv() externalcmd.Environment {
	return externalcmd.Environment{}
}

func (p *dummyPath) RemovePublisher(_ defs.PathRemovePublisherReq) {
}

func (p *dummyPath) RemoveReader(_ defs.PathRemoveReaderReq) {
}

func TestServerPublish(t *testing.T) {
	for _, ca := range []string{"basic", "digest", "basic+digest"} {
		t.Run(ca, func(t *testing.T) {
			var strm *stream.Stream
			var reader *stream.Reader
			defer func() {
				strm.RemoveReader(reader)
			}()
			dataReceived := make(chan struct{})

			n := 0

			pathManager := &test.PathManager{
				FindPathConfImpl: func(req defs.PathFindPathConfReq) (*defs.PathFindPathConfRes, error) {
					require.Equal(t, "teststream", req.AccessRequest.Name)
					require.Equal(t, "param=value", req.AccessRequest.Query)

					if ca == "basic" {
						require.Nil(t, req.AccessRequest.CustomVerifyFunc)

						if req.AccessRequest.Credentials.User == "" && req.AccessRequest.Credentials.Pass == "" {
							return nil, &auth.Error{AskCredentials: true, Wrapped: fmt.Errorf("auth error")}
						}

						require.Equal(t, "myuser", req.AccessRequest.Credentials.User)
						require.Equal(t, "mypass", req.AccessRequest.Credentials.Pass)
					} else {
						ok := req.AccessRequest.CustomVerifyFunc("myuser", "mypass")
						if n == 0 {
							require.False(t, ok)
							n++
							return nil, &auth.Error{AskCredentials: true, Wrapped: fmt.Errorf("auth error")}
						}
						require.True(t, ok)
					}

					return &defs.PathFindPathConfRes{Conf: &conf.Path{}, User: req.AccessRequest.Credentials.User}, nil
				},
				AddPublisherImpl: func(req defs.PathAddPublisherReq) (*defs.PathAddPublisherRes, error) {
					require.Equal(t, "teststream", req.AccessRequest.Name)
					require.Equal(t, "param=value", req.AccessRequest.Query)
					require.True(t, req.AccessRequest.SkipAuth)

					strm = &stream.Stream{
						Desc:              req.Desc,
						WriteQueueSize:    512,
						RTPMaxPayloadSize: 1450,
						Parent:            test.NilLogger,
					}
					err := strm.Initialize()
					require.NoError(t, err)

					subStream := &stream.SubStream{
						Stream:        strm,
						UseRTPPackets: true,
					}
					err = subStream.Initialize()
					require.NoError(t, err)

					reader = &stream.Reader{Parent: test.NilLogger}

					reader.OnData(
						strm.Desc.Medias[0],
						strm.Desc.Medias[0].Formats[0],
						func(u *unit.Unit) error {
							require.Equal(t, unit.PayloadH264{
								test.FormatH264.SPS,
								test.FormatH264.PPS,
								{5, 2, 3, 4},
							}, u.Payload)
							close(dataReceived)
							return nil
						})

					strm.AddReader(reader)

					return &defs.PathAddPublisherRes{Path: &dummyPath{}, SubStream: subStream}, nil
				},
			}

			var authMethods []rtspauth.VerifyMethod
			switch ca {
			case "basic":
				authMethods = []rtspauth.VerifyMethod{rtspauth.VerifyMethodBasic}
			case "digest":
				authMethods = []rtspauth.VerifyMethod{rtspauth.VerifyMethodDigestMD5}
			default:
				authMethods = []rtspauth.VerifyMethod{rtspauth.VerifyMethodBasic, rtspauth.VerifyMethodDigestMD5}
			}

			s := &Server{
				Address:        "127.0.0.1:8557",
				AuthMethods:    authMethods,
				ReadTimeout:    conf.Duration(10 * time.Second),
				WriteTimeout:   conf.Duration(10 * time.Second),
				WriteQueueSize: 512,
				Transports:     conf.RTSPTransports{gortsplib.ProtocolTCP: {}},
				PathManager:    pathManager,
				Parent:         test.NilLogger,
			}
			err := s.Initialize()
			require.NoError(t, err)
			defer s.Close()

			source := gortsplib.Client{}

			media0 := test.UniqueMediaH264()

			err = source.StartRecording(
				"rtsp://myuser:mypass@127.0.0.1:8557/teststream?param=value",
				&description.Session{Medias: []*description.Media{media0}})
			require.NoError(t, err)
			defer source.Close()

			err = source.WritePacketRTP(media0, &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					Marker:         true,
					PayloadType:    96,
					SequenceNumber: 123,
					Timestamp:      45343,
					SSRC:           563423,
				},
				Payload: []byte{5, 2, 3, 4},
			})
			require.NoError(t, err)

			<-dataReceived

			list, err := s.APISessionsList()
			require.NoError(t, err)
			require.Equal(t, &defs.APIRTSPSessionList{
				Items: []defs.APIRTSPSession{
					{
						ID:                 list.Items[0].ID,
						Created:            list.Items[0].Created,
						RemoteAddr:         list.Items[0].RemoteAddr,
						State:              "publish",
						Path:               "teststream",
						Query:              "param=value",
						User:               "myuser",
						InboundBytes:       list.Items[0].InboundBytes,
						InboundRTPPackets:  list.Items[0].InboundRTPPackets,
						OutboundBytes:      list.Items[0].OutboundBytes,
						BytesReceived:      list.Items[0].BytesReceived,
						BytesSent:          list.Items[0].BytesSent,
						Conns:              list.Items[0].Conns,
						RTPPacketsReceived: list.Items[0].RTPPacketsReceived,
						Transport:          ptrOf("TCP"),
						Profile:            ptrOf("AVP"),
					},
				},
			}, list)
		})
	}
}

func TestServerPublishMPEGTS(t *testing.T) {
	var strm *stream.Stream
	var reader *stream.Reader
	defer func() {
		if strm != nil && reader != nil {
			strm.RemoveReader(reader)
		}
	}()

	dataReceived := make(chan struct{})

	pathConf := &conf.Path{RTSPDemuxMpegts: true}

	pathManager := &test.PathManager{
		FindPathConfImpl: func(req defs.PathFindPathConfReq) (*defs.PathFindPathConfRes, error) {
			require.Equal(t, "teststream", req.AccessRequest.Name)
			require.Equal(t, "param=value", req.AccessRequest.Query)
			return &defs.PathFindPathConfRes{Conf: pathConf}, nil
		},
		AddPublisherImpl: func(req defs.PathAddPublisherReq) (*defs.PathAddPublisherRes, error) {
			require.Equal(t, "teststream", req.AccessRequest.Name)
			require.Equal(t, "param=value", req.AccessRequest.Query)
			require.True(t, req.AccessRequest.SkipAuth)
			require.False(t, req.UseRTPPackets)
			require.True(t, req.ReplaceNTP)
			require.Same(t, pathConf, req.ConfToCompare)
			require.Equal(t, &description.Session{Medias: []*description.Media{{
				Type: description.MediaTypeVideo,
				Formats: []format.Format{&format.H264{
					PayloadTyp:        96,
					PacketizationMode: 1,
				}},
			}}}, req.Desc)

			strm = &stream.Stream{
				Desc:              req.Desc,
				WriteQueueSize:    512,
				RTPMaxPayloadSize: 1450,
				Parent:            test.NilLogger,
			}
			err := strm.Initialize()
			require.NoError(t, err)

			subStream := &stream.SubStream{
				Stream:        strm,
				UseRTPPackets: false,
			}
			err = subStream.Initialize()
			require.NoError(t, err)

			reader = &stream.Reader{Parent: test.NilLogger}
			n := 0

			reader.OnData(
				strm.Desc.Medias[0],
				strm.Desc.Medias[0].Formats[0],
				func(u *unit.Unit) error {
					if n == 0 {
						require.Equal(t, unit.PayloadH264{
							test.FormatH264.SPS,
							test.FormatH264.PPS,
							{5, 1},
						}, u.Payload)
						close(dataReceived)
					}
					n++
					return nil
				})

			strm.AddReader(reader)

			return &defs.PathAddPublisherRes{Path: &dummyPath{}, SubStream: subStream}, nil
		},
	}

	s := &Server{
		Address:        "127.0.0.1:8557",
		ReadTimeout:    conf.Duration(10 * time.Second),
		WriteTimeout:   conf.Duration(10 * time.Second),
		WriteQueueSize: 512,
		Transports:     conf.RTSPTransports{gortsplib.ProtocolTCP: {}},
		PathManager:    pathManager,
		Parent:         test.NilLogger,
	}
	err := s.Initialize()
	require.NoError(t, err)
	defer s.Close()

	source := gortsplib.Client{}

	media0 := &description.Media{
		Type:    description.MediaTypeApplication,
		Formats: []format.Format{&format.MPEGTS{}},
	}

	err = source.StartRecording(
		"rtsp://127.0.0.1:8557/teststream?param=value",
		&description.Session{Medias: []*description.Media{media0}})
	require.NoError(t, err)
	defer source.Close()

	track := &mpegts.Track{Codec: &tscodecs.H264{}}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	w := &mpegts.Writer{W: bw, Tracks: []*mpegts.Track{track}}
	err = w.Initialize()
	require.NoError(t, err)

	// the MPEG-TS muxer needs two PES packets in order to write the first one
	err = w.WriteH264(track, 0, 0, [][]byte{
		test.FormatH264.SPS,
		test.FormatH264.PPS,
		{5, 1},
	})
	require.NoError(t, err)

	err = w.WriteH264(track, 0, 0, [][]byte{{5, 2}})
	require.NoError(t, err)

	err = bw.Flush()
	require.NoError(t, err)

	raw := buf.Bytes()
	require.NotEmpty(t, raw)
	require.Zero(t, len(raw)%188)

	tsPackets := make([][]byte, 0, len(raw)/188)
	for len(raw) > 0 {
		tsPackets = append(tsPackets, raw[:188:188])
		raw = raw[188:]
	}

	encoder, err := media0.Formats[0].(*format.MPEGTS).CreateEncoder()
	require.NoError(t, err)

	rtpPackets, err := encoder.Encode(tsPackets)
	require.NoError(t, err)

	for _, pkt := range rtpPackets {
		err = source.WritePacketRTP(media0, pkt)
		require.NoError(t, err)
	}

	<-dataReceived
}

func TestServerRead(t *testing.T) {
	for _, ca := range []string{"basic", "digest", "basic+digest"} {
		t.Run(ca, func(t *testing.T) {
			desc := &description.Session{Medias: []*description.Media{test.MediaH264}}

			strm := &stream.Stream{
				Desc:              desc,
				WriteQueueSize:    512,
				RTPMaxPayloadSize: 1450,
				Parent:            test.NilLogger,
			}
			err := strm.Initialize()
			require.NoError(t, err)

			subStream := &stream.SubStream{
				Stream:        strm,
				UseRTPPackets: false,
			}
			err = subStream.Initialize()
			require.NoError(t, err)

			n := 0

			pathManager := &test.PathManager{
				DescribeImpl: func(req defs.PathDescribeReq) defs.PathDescribeRes {
					require.Equal(t, "teststream", req.AccessRequest.Name)
					require.Equal(t, "param=value", req.AccessRequest.Query)

					if ca == "basic" {
						require.Nil(t, req.AccessRequest.CustomVerifyFunc)

						if req.AccessRequest.Credentials.User == "" && req.AccessRequest.Credentials.Pass == "" {
							return defs.PathDescribeRes{Err: &auth.Error{AskCredentials: true, Wrapped: fmt.Errorf("auth error")}}
						}

						require.Equal(t, "myuser", req.AccessRequest.Credentials.User)
						require.Equal(t, "mypass", req.AccessRequest.Credentials.Pass)
					} else {
						ok := req.AccessRequest.CustomVerifyFunc("myuser", "mypass")
						if n == 0 {
							require.False(t, ok)
							n++
							return defs.PathDescribeRes{Err: &auth.Error{AskCredentials: true, Wrapped: fmt.Errorf("auth error")}}
						}
						require.True(t, ok)
					}

					return defs.PathDescribeRes{
						Path:   &dummyPath{},
						Stream: strm,
						Err:    nil,
					}
				},
				AddReaderImpl: func(req defs.PathAddReaderReq) (*defs.PathAddReaderRes, error) {
					require.Equal(t, "teststream", req.AccessRequest.Name)
					require.Equal(t, "param=value", req.AccessRequest.Query)

					if ca == "basic" {
						require.Nil(t, req.AccessRequest.CustomVerifyFunc)
						require.Equal(t, "myuser", req.AccessRequest.Credentials.User)
						require.Equal(t, "mypass", req.AccessRequest.Credentials.Pass)
					} else {
						ok := req.AccessRequest.CustomVerifyFunc("myuser", "mypass")
						require.True(t, ok)
					}

					return &defs.PathAddReaderRes{Path: &dummyPath{}, User: req.AccessRequest.Credentials.User, Stream: strm}, nil
				},
			}

			var authMethods []rtspauth.VerifyMethod
			switch ca {
			case "basic":
				authMethods = []rtspauth.VerifyMethod{rtspauth.VerifyMethodBasic}
			case "digest":
				authMethods = []rtspauth.VerifyMethod{rtspauth.VerifyMethodDigestMD5}
			default:
				authMethods = []rtspauth.VerifyMethod{rtspauth.VerifyMethodBasic, rtspauth.VerifyMethodDigestMD5}
			}

			s := &Server{
				Address:        "127.0.0.1:8557",
				AuthMethods:    authMethods,
				ReadTimeout:    conf.Duration(10 * time.Second),
				WriteTimeout:   conf.Duration(10 * time.Second),
				WriteQueueSize: 512,
				Transports:     conf.RTSPTransports{gortsplib.ProtocolTCP: {}},
				PathManager:    pathManager,
				Parent:         test.NilLogger,
			}
			err = s.Initialize()
			require.NoError(t, err)
			defer s.Close()

			u, err := base.ParseURL("rtsp://myuser:mypass@127.0.0.1:8557/teststream?param=value")
			require.NoError(t, err)

			reader := gortsplib.Client{
				Scheme: u.Scheme,
				Host:   u.Host,
			}

			err = reader.Start()
			require.NoError(t, err)
			defer reader.Close()

			desc2, _, err := reader.Describe(u)
			require.NoError(t, err)

			err = reader.SetupAll(desc2.BaseURL, desc2.Medias)
			require.NoError(t, err)

			recv := make(chan struct{})

			reader.OnPacketRTPAny(func(_ *description.Media, _ format.Format, p *rtp.Packet) {
				require.Equal(t, &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						Marker:         true,
						PayloadType:    96,
						SequenceNumber: p.SequenceNumber,
						Timestamp:      p.Timestamp,
						SSRC:           p.SSRC,
					},
					Payload: []byte{
						0x18, 0x00, 0x19, 0x67, 0x42, 0xc0, 0x28, 0xd9,
						0x00, 0x78, 0x02, 0x27, 0xe5, 0x84, 0x00, 0x00,
						0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0,
						0x3c, 0x60, 0xc9, 0x20, 0x00, 0x04, 0x08, 0x06,
						0x07, 0x08, 0x00, 0x04, 0x05, 0x02, 0x03, 0x04,
					},
				}, p)
				close(recv)
			})

			_, err = reader.Play(nil)
			require.NoError(t, err)

			subStream.WriteUnit(desc.Medias[0], desc.Medias[0].Formats[0], &unit.Unit{
				NTP: time.Time{},
				Payload: unit.PayloadH264{
					{5, 2, 3, 4}, // IDR
				},
			})

			<-recv

			list, err := s.APISessionsList()
			require.NoError(t, err)
			require.Equal(t, &defs.APIRTSPSessionList{
				Items: []defs.APIRTSPSession{
					{
						ID:                 list.Items[0].ID,
						Created:            list.Items[0].Created,
						RemoteAddr:         list.Items[0].RemoteAddr,
						State:              "read",
						Path:               "teststream",
						Query:              "param=value",
						User:               "myuser",
						InboundBytes:       list.Items[0].InboundBytes,
						InboundRTPPackets:  list.Items[0].InboundRTPPackets,
						OutboundBytes:      list.Items[0].OutboundBytes,
						OutboundRTPPackets: list.Items[0].OutboundRTPPackets,
						BytesReceived:      list.Items[0].BytesReceived,
						BytesSent:          list.Items[0].BytesSent,
						Conns:              list.Items[0].Conns,
						RTPPacketsReceived: list.Items[0].RTPPacketsReceived,
						RTPPacketsSent:     list.Items[0].RTPPacketsSent,
						Transport:          ptrOf("TCP"),
						Profile:            ptrOf("AVP"),
					},
				},
			}, list)
		})
	}
}

func TestServerReadMulticastPathOverrides(t *testing.T) {
	pathConf := &conf.Path{
		RTSPPublishMulticastIPVideo:             "224.10.0.1",
		RTSPPublishMulticastIPAudio:             "224.10.0.2",
		RTSPPublishMulticastIPApplication:       "224.10.0.3",
		RTSPPublishMulticastRTPPortVideo:        ptrOf(10000),
		RTSPPublishMulticastRTPPortAudio:        ptrOf(10002),
		RTSPPublishMulticastRTPPortApplication:  ptrOf(10004),
		RTSPPublishMulticastRTCPPortVideo:       ptrOf(10001),
		RTSPPublishMulticastRTCPPortAudio:       ptrOf(10003),
		RTSPPublishMulticastRTCPPortApplication: ptrOf(10005),
	}

	var strm *stream.Stream

	pathManager := &test.PathManager{
		FindPathConfImpl: func(req defs.PathFindPathConfReq) (*defs.PathFindPathConfRes, error) {
			require.Equal(t, "teststream", req.AccessRequest.Name)
			return &defs.PathFindPathConfRes{Conf: pathConf}, nil
		},
		DescribeImpl: func(req defs.PathDescribeReq) defs.PathDescribeRes {
			require.Equal(t, "teststream", req.AccessRequest.Name)
			require.NotNil(t, strm)
			return defs.PathDescribeRes{
				Path:   &dummyPath{},
				Stream: strm,
			}
		},
		AddPublisherImpl: func(req defs.PathAddPublisherReq) (*defs.PathAddPublisherRes, error) {
			require.Equal(t, "teststream", req.AccessRequest.Name)
			require.True(t, req.UseRTPPackets)
			require.True(t, req.ReplaceNTP)
			require.Same(t, pathConf, req.ConfToCompare)

			strm = &stream.Stream{
				Desc:              req.Desc,
				PathConf:          pathConf,
				WriteQueueSize:    512,
				RTPMaxPayloadSize: 1450,
				Parent:            test.NilLogger,
			}
			err := strm.Initialize()
			require.NoError(t, err)

			subStream := &stream.SubStream{
				Stream:        strm,
				UseRTPPackets: true,
			}
			err = subStream.Initialize()
			require.NoError(t, err)

			return &defs.PathAddPublisherRes{Path: &dummyPath{}, SubStream: subStream}, nil
		},
		AddReaderImpl: func(req defs.PathAddReaderReq) (*defs.PathAddReaderRes, error) {
			require.Equal(t, "teststream", req.AccessRequest.Name)
			require.NotNil(t, strm)
			return &defs.PathAddReaderRes{
				Path:   &dummyPath{},
				Stream: strm,
			}, nil
		},
	}

	transports := conf.RTSPTransports{
		gortsplib.ProtocolTCP:          {},
		gortsplib.ProtocolUDPMulticast: {},
	}

	s := &Server{
		Address:           "127.0.0.1:8557",
		ReadTimeout:       conf.Duration(10 * time.Second),
		WriteTimeout:      conf.Duration(10 * time.Second),
		WriteQueueSize:    512,
		RTSPTransports:    transports,
		Transports:        transports,
		MulticastIPRange:  "224.1.0.0/16",
		MulticastRTPPort:  8002,
		MulticastRTCPPort: 8003,
		PathManager:       pathManager,
		Parent:            test.NilLogger,
	}
	err := s.Initialize()
	require.NoError(t, err)
	defer s.Close()

	source := gortsplib.Client{}

	videoMedia := test.UniqueMediaH264()
	audioMedia := test.UniqueMediaMPEG4Audio()
	applicationMedia := &description.Media{
		Type:    description.MediaTypeApplication,
		Formats: []format.Format{&format.KLV{PayloadTyp: 98}},
	}

	audioEncoder, err := audioMedia.Formats[0].(*format.MPEG4Audio).CreateEncoder()
	require.NoError(t, err)

	err = source.StartRecording(
		"rtsp://127.0.0.1:8557/teststream",
		&description.Session{Medias: []*description.Media{videoMedia, audioMedia, applicationMedia}},
	)
	require.NoError(t, err)
	defer source.Close()

	conn, err := net.Dial("tcp", "127.0.0.1:8557")
	require.NoError(t, err)
	defer conn.Close()

	br := bufio.NewReader(conn)

	u, err := base.ParseURL("rtsp://127.0.0.1:8557/teststream")
	require.NoError(t, err)

	byts, err := base.Request{
		Method: base.Describe,
		URL:    u,
		Header: base.Header{
			"CSeq": base.HeaderValue{"1"},
		},
	}.Marshal()
	require.NoError(t, err)

	_, err = conn.Write(byts)
	require.NoError(t, err)

	var describeRes base.Response
	err = describeRes.Unmarshal(br)
	require.NoError(t, err)
	require.Equal(t, base.StatusOK, describeRes.StatusCode)

	var desc sdp.SessionDescription
	err = desc.Unmarshal(describeRes.Body)
	require.NoError(t, err)
	require.Len(t, desc.MediaDescriptions, 3)

	controlURL := func(control string) string {
		if strings.HasPrefix(control, "rtsp://") || strings.HasPrefix(control, "rtsps://") {
			return control
		}
		return "rtsp://127.0.0.1:8557/teststream/" + control
	}

	expected := []struct {
		mediaType string
		ip        string
		rtpPort   int
		rtcpPort  int
	}{
		{"video", "224.10.0.1", 10000, 10001},
		{"audio", "224.10.0.2", 10002, 10003},
		{"application", "224.10.0.3", 10004, 10005},
	}

	var sessionID string

	for i, mediaDesc := range desc.MediaDescriptions {
		control, ok := mediaDesc.Attribute("control")
		require.True(t, ok)

		setupURL, err := base.ParseURL(controlURL(control))
		require.NoError(t, err)

		header := base.Header{
			"CSeq":      base.HeaderValue{fmt.Sprintf("%d", i+2)},
			"Transport": base.HeaderValue{"RTP/AVP;multicast;mode=play"},
		}
		if sessionID != "" {
			header["Session"] = base.HeaderValue{sessionID}
		}

		byts, err = base.Request{
			Method: base.Setup,
			URL:    setupURL,
			Header: header,
		}.Marshal()
		require.NoError(t, err)

		_, err = conn.Write(byts)
		require.NoError(t, err)

		var setupRes base.Response
		err = setupRes.Unmarshal(br)
		require.NoError(t, err)
		require.Equal(t, base.StatusOK, setupRes.StatusCode)

		require.Contains(t, setupRes.Header["Transport"][0], "multicast")
		require.Contains(t, setupRes.Header["Transport"][0], "destination="+expected[i].ip)
		require.Contains(t, setupRes.Header["Transport"][0],
			fmt.Sprintf("port=%d-%d", expected[i].rtpPort, expected[i].rtcpPort))

		rawSessionID, ok := setupRes.Header["Session"]
		require.True(t, ok)
		require.NotEmpty(t, rawSessionID)
		require.Equal(t, expected[i].mediaType, mediaDesc.MediaName.Media)
		sessionID = strings.Split(rawSessionID[0], ";")[0]
	}

	byts, err = base.Request{
		Method: base.Play,
		URL:    u,
		Header: base.Header{
			"CSeq":    base.HeaderValue{"5"},
			"Session": base.HeaderValue{sessionID},
		},
	}.Marshal()
	require.NoError(t, err)

	_, err = conn.Write(byts)
	require.NoError(t, err)

	var playRes base.Response
	err = playRes.Unmarshal(br)
	require.NoError(t, err)
	require.Equal(t, base.StatusOK, playRes.StatusCode)

	// Emit multiple packets per media after PLAY so external captures can
	// reliably observe traffic on each configured multicast RTP port.
	time.Sleep(300 * time.Millisecond)

	for i := range 5 {
		err = source.WritePacketRTP(videoMedia, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Marker:         true,
				PayloadType:    96,
				SequenceNumber: uint16(1200 + i),
				Timestamp:      90000 + uint32(i)*90000,
				SSRC:           0x10101010,
			},
			Payload: []byte{0x05, 0x01, 0x02, 0x03},
		})
		require.NoError(t, err)

		audioPackets, err := audioEncoder.Encode([][]byte{{0x01, 0x02, 0x03, 0x04}})
		require.NoError(t, err)
		require.Len(t, audioPackets, 1)
		audioPacket := audioPackets[0]
		audioPacket.SequenceNumber = uint16(2200 + i)
		audioPacket.Timestamp = 44100 + uint32(i)*1024
		audioPacket.SSRC = 0x20202020

		err = source.WritePacketRTP(audioMedia, audioPacket)
		require.NoError(t, err)

		err = source.WritePacketRTP(applicationMedia, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Marker:         true,
				PayloadType:    98,
				SequenceNumber: uint16(3200 + i),
				Timestamp:      12345 + uint32(i)*900,
				SSRC:           0x30303030,
			},
			Payload: []byte{
				0x06, 0x0e, 0x2b, 0x34, 0x02, 0x0b, 0x01, 0x01,
				0x0e, 0x01, 0x03, 0x01, 0x01, 0x00, 0x00, 0x00,
				0x01, 0xff,
			},
		})
		require.NoError(t, err)

		time.Sleep(50 * time.Millisecond)
	}

	time.Sleep(300 * time.Millisecond)
}

func TestServerRedirect(t *testing.T) {
	for _, ca := range []string{"relative", "absolute"} {
		t.Run(ca, func(t *testing.T) {
			desc := &description.Session{Medias: []*description.Media{test.MediaH264}}

			strm := &stream.Stream{
				Desc:              desc,
				WriteQueueSize:    512,
				RTPMaxPayloadSize: 1450,
				Parent:            test.NilLogger,
			}
			err := strm.Initialize()
			require.NoError(t, err)

			subStream := &stream.SubStream{
				Stream:        strm,
				UseRTPPackets: true,
			}
			err = subStream.Initialize()
			require.NoError(t, err)

			pathManager := &test.PathManager{
				DescribeImpl: func(req defs.PathDescribeReq) defs.PathDescribeRes {
					if req.AccessRequest.Name == "path1" {
						if ca == "relative" {
							return defs.PathDescribeRes{
								Redirect: "/path2",
							}
						}
						return defs.PathDescribeRes{
							Redirect: "rtsp://localhost:8557/path2",
						}
					}

					if req.AccessRequest.Credentials.User == "" && req.AccessRequest.Credentials.Pass == "" {
						return defs.PathDescribeRes{Err: &auth.Error{AskCredentials: true, Wrapped: fmt.Errorf("auth error")}}
					}

					require.Equal(t, "path2", req.AccessRequest.Name)
					require.Equal(t, "", req.AccessRequest.Query)
					require.Equal(t, "myuser", req.AccessRequest.Credentials.User)
					require.Equal(t, "mypass", req.AccessRequest.Credentials.Pass)

					return defs.PathDescribeRes{
						Path:   &dummyPath{},
						Stream: strm,
					}
				},
			}

			s := &Server{
				Address:        "127.0.0.1:8557",
				AuthMethods:    []rtspauth.VerifyMethod{rtspauth.VerifyMethodBasic},
				ReadTimeout:    conf.Duration(10 * time.Second),
				WriteTimeout:   conf.Duration(10 * time.Second),
				WriteQueueSize: 512,
				Transports:     conf.RTSPTransports{gortsplib.ProtocolTCP: {}},
				PathManager:    pathManager,
				Parent:         test.NilLogger,
			}
			err = s.Initialize()
			require.NoError(t, err)
			defer s.Close()

			u, err := base.ParseURL("rtsp://myuser:mypass@127.0.0.1:8557/path1?param=value")
			require.NoError(t, err)

			reader := gortsplib.Client{
				Scheme: u.Scheme,
				Host:   u.Host,
			}

			err = reader.Start()
			require.NoError(t, err)
			defer reader.Close()

			desc2, _, err := reader.Describe(u)
			require.NoError(t, err)

			require.Equal(t, desc.Medias[0].Formats, desc2.Medias[0].Formats)
		})
	}
}

func TestAuthError(t *testing.T) {
	pathManager := &test.PathManager{
		DescribeImpl: func(req defs.PathDescribeReq) defs.PathDescribeRes {
			if req.AccessRequest.Credentials.User == "" && req.AccessRequest.Credentials.Pass == "" {
				return defs.PathDescribeRes{Err: &auth.Error{AskCredentials: true, Wrapped: fmt.Errorf("auth error")}}
			}

			return defs.PathDescribeRes{Err: &auth.Error{Wrapped: fmt.Errorf("auth error")}}
		},
	}

	var n atomic.Int64
	done := make(chan struct{})

	s := &Server{
		Address:        "127.0.0.1:8557",
		ReadTimeout:    conf.Duration(10 * time.Second),
		WriteTimeout:   conf.Duration(10 * time.Second),
		WriteQueueSize: 512,
		PathManager:    pathManager,
		Parent: test.Logger(func(l logger.Level, s string, i ...any) {
			if l == logger.Info {
				if n.Add(1) == 3 {
					require.Regexp(t, "authentication failed: auth error$", fmt.Sprintf(s, i...))
					close(done)
				}
			}
		}),
	}
	err := s.Initialize()
	require.NoError(t, err)
	defer s.Close()

	u, err := base.ParseURL("rtsp://myuser:mypass@127.0.0.1:8557/teststream?param=value")
	require.NoError(t, err)

	reader := gortsplib.Client{
		Scheme: u.Scheme,
		Host:   u.Host,
	}

	err = reader.Start()
	require.NoError(t, err)
	defer reader.Close()

	_, _, err = reader.Describe(u)
	require.EqualError(t, err, "bad status code: 401 (Unauthorized)")

	<-done
}
