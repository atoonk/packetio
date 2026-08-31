package packetio

import "errors"

var (
	// ErrClosed is returned by a method on a queue or device that has been
	// closed.
	ErrClosed = errors.New("packetio: closed")

	// ErrQueueFailed is returned when the hardware reported an error that put
	// the queue out of service. The queue accepts no further work; close it and
	// open a new one.
	ErrQueueFailed = errors.New("packetio: queue failed")

	// ErrBadLength is returned when a build callback reports a length outside
	// the frame it was given. Nothing is transmitted.
	ErrBadLength = errors.New("packetio: build returned invalid length")

	// ErrUnsupported is returned for an option or operation this backend or
	// this NIC cannot provide.
	ErrUnsupported = errors.New("packetio: unsupported")
)
