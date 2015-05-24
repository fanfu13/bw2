package api

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	log "github.com/cihub/seelog"
	"github.com/immesys/bw2/internal/core"
	"github.com/immesys/bw2/internal/util"
	"github.com/immesys/bw2/objects"
)

const (
	NoElaboration      = 0
	PartialElaboration = 1
	FullElaboration    = 2
)

func init() {
	fmt.Println("init called")
	cfg := `
	<seelog>
    <outputs>
        <splitter formatid="common">
            <console/>
            <file path="bw.log"/>
        </splitter>
    </outputs>
		<formats>
				<format id="common" format="[%LEV] %Time %Date %File:%Line %Msg%n"/>
		</formats>
	</seelog>`

	nlogger, err := log.LoggerFromConfigAsString(cfg)
	if err == nil {
		log.ReplaceLogger(nlogger)
		log.Infof("Logger loaded")
	} else {
		fmt.Printf("Bad log config: %v\n", err)
		os.Exit(1)
	}
}

type PublishParams struct {
	MVK                []byte
	URISuffix          string
	PrimaryAccessChain *objects.DChain
	RoutingObjects     []objects.RoutingObject
	PayloadObjects     []objects.PayloadObject
	Expiry             *time.Time
	ExpiryDelta        *time.Duration
	ElaboratePAC       int
	DoVerify           bool
}
type PublishCallback func(status int)

func (c *BosswaveClient) Publish(params *PublishParams,
	cb PublishCallback) {
	m, code := c.newMessage(core.TypePublish, params.MVK, params.URISuffix)
	if m == nil {
		cb(code)
		return
	}
	m.PrimaryAccessChain = params.PrimaryAccessChain
	m.RoutingObjects = params.RoutingObjects
	m.PayloadObjects = params.PayloadObjects
	if s := c.doPAC(m, params.ElaboratePAC); s != core.BWStatusOkay {
		cb(s)
		return
	}

	//Add expiry
	if params.ExpiryDelta != nil {
		m.RoutingObjects = append(m.RoutingObjects, objects.CreateNewExpiryFromNow(*params.ExpiryDelta))
	} else if params.Expiry != nil {
		m.RoutingObjects = append(m.RoutingObjects, objects.CreateNewExpiry(*params.Expiry))
	}

	//Check if we need to add an origin VK header
	if m.PrimaryAccessChain == nil ||
		m.PrimaryAccessChain.GetReceiverVK() == nil ||
		objects.IsEveryoneVK(m.PrimaryAccessChain.GetReceiverVK()) {
		fmt.Println("Adding an origin VK header")
		m.RoutingObjects = append(m.RoutingObjects, objects.CreateOriginVK(c.us.GetVK()))
	}

	c.finishMessage(m)

	if params.DoVerify {
		s := m.Verify()
		if s.Code != core.BWStatusOkay {
			cb(s.Code)
			return
		}
	}
	//Probably wanna do shit like determine if this is for remote delivery or local
	c.cl.Publish(m)
	cb(core.BWStatusOkay)
}

type SubscribeParams struct {
	MVK                []byte
	URISuffix          string
	PrimaryAccessChain *objects.DChain
	RoutingObjects     []objects.RoutingObject
	ElaboratePAC       int
	DoVerify           bool
}
type SubscribeInitialCallback func(status int, isNew bool, id core.UniqueMessageID)
type SubscribeMessageCallback func(m *core.Message)

func (c *BosswaveClient) Subscribe(params *SubscribeParams,
	actionCB SubscribeInitialCallback,
	messageCB SubscribeMessageCallback) {

	m, code := c.newMessage(core.TypeSubscribe, params.MVK, params.URISuffix)
	if m == nil {
		actionCB(code, false, core.UniqueMessageID{})
		return
	}
	m.PrimaryAccessChain = params.PrimaryAccessChain
	m.RoutingObjects = params.RoutingObjects
	if s := c.doPAC(m, params.ElaboratePAC); s != core.BWStatusOkay {
		actionCB(s, false, core.UniqueMessageID{})
		return
	}
	//Check if we need to add an origin VK header
	if m.PrimaryAccessChain == nil ||
		m.PrimaryAccessChain.GetReceiverVK() == nil ||
		objects.IsEveryoneVK(m.PrimaryAccessChain.GetReceiverVK()) {
		m.RoutingObjects = append(m.RoutingObjects, objects.CreateOriginVK(c.us.GetVK()))
	}

	c.finishMessage(m)

	if params.DoVerify {
		s := m.Verify()
		if s.Code != core.BWStatusOkay {
			actionCB(s.Code, false, core.UniqueMessageID{})
			return
		}
	}
	subid := c.cl.Subscribe(m, func(m *core.Message, subid core.UniqueMessageID) {
		messageCB(m)
	})
	isNew := subid == m.UMid
	actionCB(core.BWStatusOkay, isNew, subid)
}

type CreateDotParams struct {
	IsPermission     bool
	To               []byte
	TTL              uint8
	Expiry           *time.Time
	ExpiryDelta      *time.Duration
	Contact          string
	Comment          string
	Revokers         [][]byte
	OmitCreationDate bool

	//For Access
	URISuffix         string
	MVK               []byte
	AccessPermissions string

	//For Permissions
	Permissions map[string]string
}

func (c *BosswaveClient) CreateDOT(p *CreateDotParams) *objects.DOT {
	if len(p.To) != 32 {
		log.Info("To VK bad")
		return nil
	}
	d := objects.CreateDOT(!p.IsPermission, c.us.GetVK(), p.To)
	d.SetTTL(int(p.TTL))
	d.SetContact(p.Contact)
	d.SetComment(p.Comment)
	if p.ExpiryDelta != nil {
		d.SetExpiry(time.Now().Add(*p.ExpiryDelta))
	} else if p.Expiry != nil {
		d.SetExpiry(*p.Expiry)
	}
	if !p.OmitCreationDate {
		d.SetCreationToNow()
	}
	for _, r := range p.Revokers {
		if len(r) != 32 {
			log.Info("Delegated revoker bad")
			return nil
		}
		d.AddRevoker(r)
	}
	if p.IsPermission {
		for k, v := range p.Permissions {
			d.SetPermission(k, v)
		}
	} else {
		d.SetAccessURI(p.MVK, p.URISuffix)
		if !d.SetPermString(p.AccessPermissions) {
			log.Info("Failed to set access permissions")
			return nil
		}
	}
	d.Encode(c.us.GetSK())
	return d
}

type CreateDotChainParams struct {
	DOTs         []*objects.DOT
	IsPermission bool
	UnElaborate  bool
}

func (c *BosswaveClient) CreateDotChain(p *CreateDotChainParams) *objects.DChain {
	rv, err := objects.CreateDChain(!p.IsPermission, p.DOTs...)
	if err != nil || rv == nil {
		return nil
	}
	if p.UnElaborate {
		rv.UnElaborate()
	}
	return rv
}

func (c *BosswaveClient) doPAC(m *core.Message, elaboratePAC int) int {

	//If there is no explicit PAC, use the first access chain in the ROs
	if m.PrimaryAccessChain == nil {
		for _, ro := range m.RoutingObjects {
			if ro.GetRONum() == objects.ROAccessDChain ||
				ro.GetRONum() == objects.ROAccessDChainHash {
				m.PrimaryAccessChain = ro.(*objects.DChain)
				break
			}
		}
	}
	//Elaborate PAC
	if elaboratePAC > NoElaboration {
		if m.PrimaryAccessChain == nil {
			return core.BWStatusUnresolvable
		}
		if !m.PrimaryAccessChain.IsElaborated() {
			dc := core.ElaborateDChain(m.PrimaryAccessChain)
			if dc == nil {
				return core.BWStatusUnresolvable
			}
			m.RoutingObjects = append(m.RoutingObjects, dc)
		}
		if elaboratePAC > PartialElaboration {
			ok := core.ResolveDotsInDChain(m.PrimaryAccessChain, m.RoutingObjects)
			if !ok {
				return core.BWStatusUnresolvable
			}
			for i := 0; i < m.PrimaryAccessChain.NumHashes(); i++ {
				d := m.PrimaryAccessChain.GetDOT(i)
				if d != nil {
					m.RoutingObjects = append(m.RoutingObjects, d)
				}
			}
		}
	} else if m.PrimaryAccessChain != nil {
		m.PrimaryAccessChain.UnElaborate()
	}

	if m.PrimaryAccessChain != nil {
		m.RoutingObjects = append(m.RoutingObjects, m.PrimaryAccessChain)
	}
	//TODO remove duplicates in the routing objects, but preserve order.
	return core.BWStatusOkay
}

func (c *BosswaveClient) getMid() uint64 {
	mid := atomic.AddUint64(&c.mid, 1)
	return mid
}

func (c *BosswaveClient) newMessage(mtype int, mvk []byte, urisuffix string) (*core.Message, int) {
	m := core.Message{Type: uint8(mtype),
		TopicSuffix:    urisuffix,
		MVK:            mvk,
		RoutingObjects: []objects.RoutingObject{},
		PayloadObjects: []objects.PayloadObject{},
		MessageID:      c.getMid()}
	valid, star, plus, _, _ := util.AnalyzeSuffix(urisuffix)
	if !valid {
		return nil, core.BWStatusBadURI
	} else if len(mvk) != 32 {
		return nil, core.BWStatusBadURI
	} else if (star || plus) && (mtype == core.TypePublish || mtype == core.TypePersist) {
		return nil, core.BWStatusBadOperation
	}
	return &m, core.BWStatusOkay
}

func (c *BosswaveClient) finishMessage(m *core.Message) {
	m.Encode(c.us.GetSK(), c.us.GetVK())
	m.Topic = base64.URLEncoding.EncodeToString(m.MVK) + "/" + m.TopicSuffix
	m.UMid.Mid = m.MessageID
	m.UMid.Sig = binary.LittleEndian.Uint64(m.Signature)
}
