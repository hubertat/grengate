# grengate

Apple HomeKit gateway for Grenton home automation systems, written in GO.

Based on great [https://github.com/brutella/hc](https://github.com/brutella/hc) framework.


## what it does

It is a gateway between Grenton home automation system and HomeKit.
In other words: it makes possible to controll Grenton devices in Apple HomeKit system.

Of course other things are needed:

## what is needed

0. Device with *grengate* program running
1. Grenton system
2. Grenton GATE module (HTTP Gate)
3. Lua script running on Grenton Gate module (provided here, read below)

+ some configuration

## configuration

### grengate

### grenton GATE

#### http listener

#### lua script


## changelog

### v0.3

Added thermostat object, did some code refactor.

### v0.2

No more js Homebridge! Using [https://github.com/brutella/hc](https://github.com/brutella/hc) package it is a standalone app acting as HomeKit accessory and connecting to Grenton system.

So far only simple Light object present, using only Grenton DOUT.

### v0.1

First working version, it is a queue between node.js Homebridge and Grenton Gate.
Gate module simply couldn't keep up with many http requests, so I made this go app.