package conntrack

import (
	"fmt"
	"net"

	"github.com/mdlayher/netlink"

	"github.com/ti-mo/netfilter"
)

// Flow represents a snapshot of a Conntrack connection.
type Flow struct {
	ID        uint32
	Timeout   uint32
	Timestamp Timestamp

	Status    Status
	ProtoInfo ProtoInfo
	Helper    Helper

	Zone uint16

	CountersOrig, CountersReply Counter

	SecurityContext Security

	TupleOrig, TupleReply, TupleMaster Tuple

	SeqAdjOrig, SeqAdjReply SequenceAdjust

	Labels, LabelsMask []byte

	Mark, Use uint32

	SynProxy SynProxy
}

// NewFlow returns a new Flow object with the minimum necessary attributes to create a Conntrack entry.
// Writes values into the Status, Timeout, TupleOrig and TupleReply fields of the Flow.
//
// proto is the layer 4 protocol number of the connection.
// status is a StatusFlag value, or an ORed combination thereof.
// srcAddr and dstAddr are the source and destination addresses.
// srcPort and dstPort are the source and destination ports.
// timeout is the non-zero time-to-live of a connection in seconds.
func NewFlow(proto uint8, status StatusFlag, srcAddr, destAddr net.IP, srcPort, destPort uint16, timeout, mark uint32) Flow {

	var f Flow

	f.Status.Value = status

	f.Timeout = timeout
	f.Mark = mark

	f.TupleOrig.IP.SourceAddress = srcAddr
	f.TupleOrig.IP.DestinationAddress = destAddr
	f.TupleOrig.Proto.SourcePort = srcPort
	f.TupleOrig.Proto.DestinationPort = destPort
	f.TupleOrig.Proto.Protocol = proto

	// Set up TupleReply with source and destination inverted
	f.TupleReply.IP.SourceAddress = destAddr
	f.TupleReply.IP.DestinationAddress = srcAddr
	f.TupleReply.Proto.SourcePort = destPort
	f.TupleReply.Proto.DestinationPort = srcPort
	f.TupleReply.Proto.Protocol = proto

	return f
}

// unmarshal unmarshals a list of netfilter.Attributes into a Flow structure.
func (f *Flow) unmarshal(attrs []netfilter.Attribute) error {

	for _, attr := range attrs {

		switch at := AttributeType(attr.Type); at {

		// CTA_TIMEOUT is the time until the Conntrack entry is automatically destroyed.
		case CTATimeout:
			f.Timeout = attr.Uint32()
		// CTA_ID is the tuple hash value generated by the kernel. It can be relied on for flow identification.
		case CTAID:
			f.ID = attr.Uint32()
		// CTA_USE is the flow's kernel-internal refcount.
		case CTAUse:
			f.Use = attr.Uint32()
		// CTA_MARK is the connection's connmark
		case CTAMark:
			f.Mark = attr.Uint32()
		// CTA_ZONE describes the Conntrack zone the flow is placed in. This can be combined with a CTA_TUPLE_ZONE
		// to specify which zone an event originates from.
		case CTAZone:
			f.Zone = attr.Uint16()
		// CTA_LABELS is a binary bitfield attached to a connection that is sent in
		// events when changed, as well as in response to dump queries.
		case CTALabels:
			f.Labels = attr.Data
		// CTA_LABELS_MASK is never sent by the kernel, but it can be used
		// in set / update queries to mask label operations on the kernel state table.
		// it needs to be exactly as wide as the CTA_LABELS field it intends to mask.
		case CTALabelsMask:
			f.LabelsMask = attr.Data
		// CTA_TUPLE_* attributes are nested and contain source and destination values for:
		// - the IPv4/IPv6 addresses involved
		// - ports used in the connection
		// - (optional) the Conntrack Zone of the originating/replying side of the flow
		case CTATupleOrig:
			if err := f.TupleOrig.UnmarshalAttribute(attr); err != nil {
				return err
			}
		case CTATupleReply:
			if err := f.TupleReply.UnmarshalAttribute(attr); err != nil {
				return err
			}
		case CTATupleMaster:
			if err := f.TupleMaster.UnmarshalAttribute(attr); err != nil {
				return err
			}
		// CTA_STATUS is a bitfield of the state of the connection
		// (eg. if packets are seen in both directions, etc.)
		case CTAStatus:
			if err := f.Status.UnmarshalAttribute(attr); err != nil {
				return err
			}
		// CTA_PROTOINFO is sent for TCP, DCCP and SCTP protocols only. It conveys extra metadata
		// about the state flags seen on the wire. Update events are sent when these change.
		case CTAProtoInfo:
			if err := f.ProtoInfo.UnmarshalAttribute(attr); err != nil {
				return err
			}
		case CTAHelp:
			if err := f.Helper.UnmarshalAttribute(attr); err != nil {
				return err
			}
		// CTA_COUNTERS_* attributes are nested and contain byte and packet counters for flows in either direction.
		case CTACountersOrig:
			if err := f.CountersOrig.UnmarshalAttribute(attr); err != nil {
				return err
			}
		case CTACountersReply:
			if err := f.CountersReply.UnmarshalAttribute(attr); err != nil {
				return err
			}
		// CTA_SECCTX is the SELinux security context of a Conntrack entry.
		case CTASecCtx:
			if err := f.SecurityContext.UnmarshalAttribute(attr); err != nil {
				return err
			}
		// CTA_TIMESTAMP is a nested attribute that describes the start and end timestamp of a flow.
		// It is sent by the kernel with dumps and DESTROY events.
		case CTATimestamp:
			if err := f.Timestamp.UnmarshalAttribute(attr); err != nil {
				return err
			}
		// CTA_SEQADJ_* is generalized TCP window adjustment metadata. It is not (yet) emitted in Conntrack events.
		// The reason for its introduction is outlined in https://lwn.net/Articles/563151.
		// Patch set is at http://www.spinics.net/lists/netdev/msg245785.html.
		case CTASeqAdjOrig:
			if err := f.SeqAdjOrig.UnmarshalAttribute(attr); err != nil {
				return err
			}
		case CTASeqAdjReply:
			if err := f.SeqAdjReply.UnmarshalAttribute(attr); err != nil {
				return err
			}
		// CTA_SYNPROXY are the connection's SYN proxy parameters
		case CTASynProxy:
			if err := f.SynProxy.UnmarshalAttribute(attr); err != nil {
				return err
			}
		default:
			return fmt.Errorf(errAttributeUnknown, at)
		}
	}

	return nil
}

// marshal marshals a Flow object into a list of netfilter.Attributes.
func (f Flow) marshal() ([]netfilter.Attribute, error) {

	// Each connection sent to the kernel should have at least an original and reply tuple.
	if !f.TupleOrig.Filled() || !f.TupleReply.Filled() {
		return nil, errNeedTuples
	}

	attrs := make([]netfilter.Attribute, 2, 12)

	to, err := f.TupleOrig.MarshalAttribute(CTATupleOrig)
	if err != nil {
		return nil, err
	}
	attrs[0] = to

	tr, err := f.TupleReply.MarshalAttribute(CTATupleReply)
	if err != nil {
		return nil, err
	}
	attrs[1] = tr

	// Optional attributes appended to the list when filled
	if f.Timeout != 0 {
		attrs = append(attrs, Num32{Value: f.Timeout}.MarshalAttribute(CTATimeout))
	}

	if f.Status.Value != 0 {
		attrs = append(attrs, f.Status.MarshalAttribute())
	}

	if f.Mark != 0 {
		attrs = append(attrs, Num32{Value: f.Mark}.MarshalAttribute(CTAMark))
	}

	if f.Zone != 0 {
		attrs = append(attrs, Num16{Value: f.Zone}.MarshalAttribute(CTAZone))
	}

	if f.ProtoInfo.Filled() {
		attrs = append(attrs, f.ProtoInfo.MarshalAttribute())
	}

	if f.Helper.Filled() {
		attrs = append(attrs, f.Helper.MarshalAttribute())
	}

	if f.TupleMaster.Filled() {
		tm, err := f.TupleMaster.MarshalAttribute(CTATupleMaster)
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, tm)
	}

	if f.SeqAdjOrig.Filled() {
		attrs = append(attrs, f.SeqAdjOrig.MarshalAttribute())
	}

	if f.SeqAdjReply.Filled() {
		attrs = append(attrs, f.SeqAdjReply.MarshalAttribute())
	}

	if f.SynProxy.Filled() {
		attrs = append(attrs, f.SynProxy.MarshalAttribute())
	}

	return attrs, nil
}

// unmarshalFlow unmarshals a Flow from a netlink.Message.
// The Message must contain valid attributes.
func unmarshalFlow(nlm netlink.Message) (Flow, error) {

	var f Flow

	_, qattrs, err := netfilter.UnmarshalNetlink(nlm)
	if err != nil {
		return f, err
	}

	err = f.unmarshal(qattrs)
	if err != nil {
		return f, err
	}

	return f, nil
}

// unmarshalFlows unmarshals a list of flows from a list of Netlink messages.
// This method can be used to parse the result of a dump or get query.
func unmarshalFlows(nlm []netlink.Message) ([]Flow, error) {

	// Pre-allocate to avoid re-allocating output slice on every op
	out := make([]Flow, 0, len(nlm))

	for i := 0; i < len(nlm); i++ {

		f, err := unmarshalFlow(nlm[i])
		if err != nil {
			return nil, err
		}

		out = append(out, f)
	}

	return out, nil
}
