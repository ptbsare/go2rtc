package miss

import (
	"fmt"
	"net/url"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/pion/rtp"
)

type Producer struct {
	core.Connection
	client *Client
}

func Dial(rawURL string) (core.Producer, error) {
	client, err := NewClient(rawURL)
	if err != nil {
		return nil, err
	}

	u, _ := url.Parse(rawURL)
	query := u.Query()

	err = client.StartMedia(query.Get("channel"), query.Get("subtype"), query.Get("audio"))
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	medias, err := probe(client, query.Get("audio") != "0")
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	return &Producer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "xiaomi/miss",
			Protocol:   client.Protocol(),
			RemoteAddr: client.RemoteAddr().String(),
			UserAgent:  client.Version(),
			Medias:     medias,
			Transport:  client,
		},
		client: client,
	}, nil
}

func probe(client *Client, audio bool) ([]*core.Media, error) {
	_ = client.SetDeadline(time.Now().Add(15 * time.Second))

	var vcodec, acodec *core.Codec

	for {
		pkt, err := client.ReadPacket()
		if err != nil {
			if vcodec != nil {
				err = fmt.Errorf("no audio")
			} else if acodec != nil {
				err = fmt.Errorf("no video")
			}
			return nil, fmt.Errorf("xiaomi: probe: %w", err)
		}

		switch pkt.CodecID {
		case codecH264:
			if vcodec == nil {
				buf := annexb.EncodeToAVCC(pkt.Payload)
				if h264.NALUType(buf) == h264.NALUTypeSPS {
					vcodec = h264.AVCCToCodec(buf)
				}
			}
		case codecH265:
			if vcodec == nil {
				buf := annexb.EncodeToAVCC(pkt.Payload)
				if h265.NALUType(buf) == h265.NALUTypeVPS {
					vcodec = h265.AVCCToCodec(buf)
				}
			}
		case codecPCMA:
			if acodec == nil {
				acodec = &core.Codec{Name: core.CodecPCMA, ClockRate: pkt.SampleRate()}
			}
		case codecOPUS:
			if acodec == nil {
				acodec = &core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2}
			}
		}

		if vcodec != nil && (acodec != nil || !audio) {
			break
		}
	}

	_ = client.SetDeadline(time.Time{})

	medias := []*core.Media{
		{
			Kind:      core.KindVideo,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{vcodec},
		},
	}

	if acodec != nil {
		medias = append(medias, &core.Media{
			Kind:      core.KindAudio,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{acodec},
		})

		medias = append(medias, &core.Media{
			Kind:      core.KindAudio,
			Direction: core.DirectionSendonly,
			Codecs:    []*core.Codec{acodec.Clone()},
		})
	}

	return medias, nil
}

const timestamp40ms = 48000 * 0.040

func (p *Producer) Start() error {
	var audioTS uint32

	type packetResult struct {
		pkt *Packet
		err error
	}
	pktCh := make(chan packetResult, 1)

	go func() {
		for {
			_ = p.client.SetDeadline(time.Now().Add(30 * time.Second))
			pkt, err := p.client.ReadPacket()
			if err != nil {
				select {
				case pktCh <- packetResult{err: err}:
				default:
				}
				return
			}
			select {
			case pktCh <- packetResult{pkt: pkt}:
			default:
				return
			}
		}
	}()

	for {
		select {
		case res := <-pktCh:
			if res.err != nil {
				return res.err
			}
			pkt := res.pkt

			p.Recv += len(pkt.Payload)

			var name string
			var pkt2 *core.Packet

			switch pkt.CodecID {
			case codecH264, codecH265:
				pkt2 = &core.Packet{
					Header: rtp.Header{
						SequenceNumber: uint16(pkt.Sequence),
						Timestamp:      TimeToRTP(pkt.Timestamp, 90000),
					},
					Payload: annexb.EncodeToAVCC(pkt.Payload),
				}
				if pkt.CodecID == codecH264 {
					name = core.CodecH264
				} else {
					name = core.CodecH265
				}
			case codecPCMA:
				name = core.CodecPCMA
				pkt2 = &core.Packet{
					Header: rtp.Header{
						Version:        2,
						Marker:         true,
						SequenceNumber: uint16(pkt.Sequence),
						Timestamp:      audioTS,
					},
					Payload: pkt.Payload,
				}
				audioTS += uint32(len(pkt.Payload))
			case codecOPUS:
				name = core.CodecOpus
				pkt2 = &core.Packet{
					Header: rtp.Header{
						Version:        2,
						Marker:         true,
						SequenceNumber: uint16(pkt.Sequence),
						Timestamp:      audioTS,
					},
					Payload: pkt.Payload,
				}
				audioTS += timestamp40ms
			}

			for _, recv := range p.Receivers {
				if recv.Codec.Name == name {
					recv.WriteRTP(pkt2)
					break
				}
			}

		case <-time.After(35 * time.Second):
			return fmt.Errorf("xiaomi: no data received for 35s, triggering reconnect")
		}
	}
}

func (p *Producer) Stop() error {
	_ = p.client.StopMedia()
	_ = p.client.Close()
	return p.Connection.Stop()
}

func TimeToRTP(timeMS, clockRate uint64) uint32 {
	return uint32(timeMS * clockRate / 1000)
}
