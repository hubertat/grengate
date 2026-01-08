# Grenton configuration

Below is grenton system configuration described, for grengate app to work properly.

## modules required

Grengate app is communicating with grenton system with HTTP requests, module needed for this is GATE HTTP grenton module.
It is capable of sending and receiving HTTP requests to and from the grenton system.
Each endpoint needs configuration, we use two endpoints:
1. Read status endpoint: for updating grengate about grenton object status
2. Update state endpoint: for updating grenton objects state

Important is limitation of grenton GATE HTTP module (or maybe of whole grenton system) for processing requests.
It cannot process too large requests and processing multiple objects takes time - sometime too long for successfull cooperation with other systems (like Homekit).

### read script

Read script contains code ran by grenton GATE HTTP module upon receiving a request: read status endpoint.
This code is responsible for parsing the request -> reading grenton object status -> preparing response -> sending response back to grengate app.
Both request and response are json objects.

### update script

Update script contains code ran by grenton GATE HTTP module upon receiving a request: update state endpoint.
