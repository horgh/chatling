- If there's no webclient connected for the IRC client we still
  need to process messages from the IRC client. Otherwise we'll end up
  blocking on the channels there and ceasing to respond to PINGs.
- We need to be able to reap dead clients.
- Retrieve history upon connecting to an already existing client.
- Deal with dependency on boxcat. We should depend on godrop or something.
  That repo is not appropriate (but is convenient for the time being).
- Make messages without /command go to current channel. Do this in the
  frontend.
