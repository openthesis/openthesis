CC  ?= gcc
AR  ?= ar

CFLAGS = -std=c99 -Wall -Wextra -fPIC -O2

all: libopentesis.a

libopentesis.a: openthesis.o
	$(AR) rcs $@ $^

openthesis.o: openthesis.c openthesis.h

clean:
	rm -f *.o *.a

.PHONY: all clean
