#include "textflag.h"

// func T0(p unsafe.Pointer)
TEXT ·T0(SB), NOSPLIT, $0-8
	MOVD	p+0(FP), R0
	PRFM	(R0), PLDL1KEEP
	RET
