#include "textflag.h"

// func T0(p unsafe.Pointer)
TEXT ·T0(SB), NOSPLIT, $0-8
	MOVQ	p+0(FP), DI
	PREFETCHT0	(DI)
	RET
