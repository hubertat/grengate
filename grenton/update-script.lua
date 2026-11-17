-- Update (write) script, GATE HTTP object "homebridge".
-- Contract: the response has the shape of the request (see README.md).
--   object in -> acknowledgement object out
--   array in  -> array out, one acknowledgement per item, same order, no holes
-- An acknowledgement is {Clu, Id, Kind} plus Error (string) when the item failed.
-- There is no read-back after a set.
-- Errors raised here use level 0, so Error carries the bare message without a
-- "[string ...]:<line>:" prefix; errors raised by execute() are passed on as they are.

local req = GATE_HTTP->homebridge->RequestBody

function SetLight(clu, id, light)
	-- temporary workaround for fibaro wall plug
	if id == "TMP0001" then
		if light.State == true then
			CLU_GRENTON_Rs->fib_wallplug1_switch(true)
		else
			CLU_GRENTON_Rs->fib_wallplug1_switch(false)
		end
		return
	end

	if light.State == true then
		_G[clu]:execute(0, id .. ":set(0, 1)")
	else
		_G[clu]:execute(0, id .. ":set(0, 0)")
	end
end

-- Thermostat features set by a command, in the order they are set.
local ThermoFields = {
	{ name = "TempSetpoint", feature = 3 },
	{ name = "State", feature = 6 },
	{ name = "Mode", feature = 8 },
}

function SetThermo(clu, id, thermo)
	-- Only the fields present in the command are set. All of them are checked before the
	-- first one is set, so a bad item changes nothing.
	local sets = {}
	for _, f in ipairs(ThermoFields) do
		if thermo[f.name] ~= nil then
			local v = tonumber(thermo[f.name])
			if v == nil then error(f.name .. " is not a number", 0) end
			table.insert(sets, id .. ":set(" .. f.feature .. ", " .. v .. ")")
		end
	end
	if #sets == 0 then
		error("Thermo command has no fields", 0)
	end
	for _, cmd in ipairs(sets) do
		_G[clu]:execute(0, cmd)
	end
end

function SetShutter(clu, id, cmd)

	if cmd == "MOVEUP" then
		_G[clu]:execute(0, id .. ":execute(0, 0)")
	end
	if cmd == "MOVEDOWN" then
		_G[clu]:execute(0, id .. ":execute(1, 0)")
	end
	if cmd == "STOP" then
		_G[clu]:execute(0, id .. ":execute(3, 0)")
	end

end


-- Runs one request item and returns its acknowledgement. Never raises.
function ProcessItem(item)
	local ack = {}
	local ok, err

	if type(item) ~= "table" then
		ack.Error = "request item is not an object"
		return ack
	end

	ack.Clu = item.Clu
	ack.Id = item.Id
	ack.Kind = item.Kind

	ok, err = pcall(function()
		if item.Clu == nil or _G[item.Clu] == nil then
			error("unknown CLU: " .. tostring(item.Clu), 0)
		end
		if item.Id == nil then
			error("missing Id", 0)
		end

		if item.Kind == "Light" then
			if type(item.Light) ~= "table" then error("missing Light payload", 0) end
			if type(item.Light.State) ~= "boolean" then error("Light.State is not a boolean", 0) end
			SetLight(item.Clu, item.Id, item.Light)
		elseif item.Kind == "Thermo" then
			if type(item.Thermo) ~= "table" then error("missing Thermo payload", 0) end
			SetThermo(item.Clu, item.Id, item.Thermo)
		elseif item.Kind == "Shutter" then
			if item.Cmd ~= "MOVEUP" and item.Cmd ~= "MOVEDOWN" and item.Cmd ~= "STOP" then
				error("unknown shutter Cmd: " .. tostring(item.Cmd), 0)
			end
			SetShutter(item.Clu, item.Id, item.Cmd)
		else
			error("unknown Kind: " .. tostring(item.Kind), 0)
		end
	end)

	if not ok then
		ack.Error = tostring(err)
	end

	return ack
end

local resp

if type(req) ~= "table" then
	resp = { Error = "invalid request body" }
elseif req[1] ~= nil then
	-- array in, array out (also for a single element)
	resp = {}
	for ix, item in ipairs(req) do
		table.insert(resp, ProcessItem(item))
	end
elseif req.Clu ~= nil or req.Kind ~= nil or req.Id ~= nil then
	-- object in, object out
	resp = ProcessItem(req)
else
	resp = { Error = "empty or unrecognised request body" }
end

GATE_HTTP->homebridge->SetResponseBody(resp)
GATE_HTTP->homebridge->SendResponse()
