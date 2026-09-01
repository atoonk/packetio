package packetio

// TimestampReceiver is implemented by a receive queue whose device records when
// each packet arrived. Use it through a type assertion:
//
//	if r, ok := rq.(packetio.TimestampReceiver); ok {
//	        descs, ts := r.ReceiveTimestamps(64)
//	}
//
// The point of a timestamp taken by the device is that it is not a measurement
// of this program. A NIC stamps a frame as it arrives at the port, before the
// DMA, before the completion, before any of this code runs; the interval
// between two of them is what happened on the wire, and it stays true however
// long a receive loop was busy elsewhere. Reading the clock in the receive loop
// instead measures the loop.
//
// [Capabilities.RxTimestamps] is the authority on whether a device really
// stamps, and is worth asking first: a queue may carry the method while its
// device has no clock, and it then returns nothing rather than inventing
// zeroes.
type TimestampReceiver interface {
	RxQueue

	// ReceiveTimestamps is Receive, and additionally returns the arrival time
	// of each packet in nanoseconds. Both slices have the same length and are
	// reused by the next call, exactly as Receive's is.
	//
	// The clock is the device's, not the wall's: it advances with an epoch
	// nobody promises, so two timestamps from one queue may be subtracted and
	// a timestamp on its own means nothing. Comparing across devices needs
	// them disciplined to a common source, which this package does not do,
	// and neither does anything here tell you when a packet was SENT: no such
	// time travels in a packet. What can be measured is time on this machine
	// -- how long a packet was held before it went back out, how long it sat
	// between the device and this code, and how evenly traffic arrived.
	//
	// Both slices belong to the queue and are overwritten by the next receive
	// call on it, including a plain Receive. Copy what must outlive that.
	//
	// A timestamp says when a packet ARRIVED, which is not the same as the
	// order it was delivered in. A NIC stamps at the port and places into the
	// queue afterwards, and when it is dropping -- offered more than it can
	// place -- the two orders come apart: measured on a ConnectX-6 Dx, stamps
	// rise packet by packet at rates the card keeps up with, and about a third
	// of them arrive out of order once it is discarding two thirds of the
	// wire. That is the hardware answering honestly about arrival, so code
	// that needs ordering must sort, and code measuring the wire should prefer
	// the stamps to the order they came in.
	ReceiveTimestamps(max int) ([]Desc, []uint64)
}
