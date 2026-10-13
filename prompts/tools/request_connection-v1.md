Asks the person to connect a service the installation offers and they have not connected yet, such as a code host, so that the session can reach it. The connections block lists these services and their names.

Use it when the task cannot go on without the service: cloning or pushing to a repository on that host, or reading or changing what lives in the service. Do not use it for a service that is connected already, or for one the task can do without.

Inputs:
- `name`: the service's name, as the connections block lists it.
- `reason`: one sentence of at most 200 characters that tells the person what you need the service for, such as "to clone acme/app and open a pull request with the fix".

The person connects the service outside this conversation, and it reaches the session from their next message. After the call, end your turn with one sentence that says what you will do once it is connected.
