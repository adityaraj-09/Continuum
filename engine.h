#ifndef CONTINUUM_ENGINE_H
#define CONTINUUM_ENGINE_H

#include <stdbool.h>
#include <stdint.h>

#include "md.h"

uint64_t nsec_now(void);
void stamp_add(struct stamp *s, uint64_t ns);

bool udp_payload(const void *packet, uint32_t len,
		 const uint8_t **payload, uint32_t *payload_len);
bool md_parse(const uint8_t *payload, uint32_t payload_len,
	      struct md_event *ev);
bool reply_path_from_rx(const void *packet, uint32_t len,
			struct reply_path *path);

void book_init(struct book *b);
void book_apply(struct book *b, const struct md_event *ev);
bool book_top(const struct book *b, int32_t *bid, uint32_t *bid_sz,
	      int32_t *ask, uint32_t *ask_sz);

bool strategy_decide(const struct book *b, uint32_t *seq,
		     struct order_intent *out);

int order_frame_write(uint8_t *dst, int dst_len,
		      const struct reply_path *path, uint16_t dst_port,
		      uint16_t ip_id, const struct order_intent *in);

#endif
