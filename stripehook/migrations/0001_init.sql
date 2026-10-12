-- Every Stripe event delivered, once each.
create table if not exists stripe_event (
    id           text primary key not null, -- evt_...
    type         text not null,             -- e.g. account.updated
    account      text,                      -- connected account (acct_...), for Connect events
    livemode     integer not null,          -- 0/1
    api_version  text,
    created      integer not null,          -- unix seconds, when Stripe created the event
    received_at  integer not null,          -- unix seconds
    processed_at integer,                   -- unix seconds the handlers ran; null until then
    payload      text not null              -- the event's JSON, as delivered
);

-- The latest copy of each object an event carried, as data.object.
create table if not exists stripe_object (
    id            text primary key not null, -- acct_..., pi_..., cus_...
    type          text not null,             -- the object's "object" field, e.g. account
    account       text,                      -- connected account, for Connect events
    data          text not null,             -- the object's JSON
    event_id      text not null,             -- the event it came from
    event_created integer not null,          -- that event's created; older events don't overwrite
    updated_at    integer not null           -- unix seconds
);
