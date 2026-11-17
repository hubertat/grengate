-- Update (write) script, GATE HTTP object "homebridge".
-- Contract: the response has the shape of the request (see README.md).
--   object in -> acknowledgement object out
--   array in  -> array out, one acknowledgement per item, same order, no holes
-- An acknowledgement is {Clu, Id, Kind} plus Error (string) when the item failed.
-- There is no read-back after a set.

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

function SetThermo(clu, id, thermo)
	local n = 0

	-- only the fields present in the command are set
	if thermo.TempSetpoint ~= nil then
		local v = tonumber(thermo.TempSetpoint)
		if v == nil then error("TempSetpoint is not a number") end
		_G[clu]:execute(0, id .. ":set(3, " .. v .. ")")
		n = n + 1
	end
	if thermo.State ~= nil then
		local v = tonumber(thermo.State)
		if v == nil then error("State is not a number") end
		_G[clu]:execute(0, id .. ":set(6, " .. v .. ")")
		n = n + 1
	end
	if thermo.Mode ~= nil then
		local v = tonumber(thermo.Mode)
		if v == nil then error("Mode is not a number") end
		_G[clu]:execute(0, id .. ":set(8, " .. v .. ")")
		n = n + 1
	end

	if n == 0 then
		error("Thermo command has no fields")
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
			error("unknown CLU: " .. tostring(item.Clu))
		end
		if item.Id == nil then
			error("missing Id")
		end

		if item.Kind == "Light" then
			if type(item.Light) ~= "table" then error("missing Light payload") end
			SetLight(item.Clu, item.Id, item.Light)
		elseif item.Kind == "Thermo" then
			if type(item.Thermo) ~= "table" then error("missing Thermo payload") end
			SetThermo(item.Clu, item.Id, item.Thermo)
		elseif item.Kind == "Shutter" then
			if item.Cmd ~= "MOVEUP" and item.Cmd ~= "MOVEDOWN" and item.Cmd ~= "STOP" then
				error("unknown shutter Cmd: " .. tostring(item.Cmd))
			end
			SetShutter(item.Clu, item.Id, item.Cmd)
		else
			error("unknown Kind: " .. tostring(item.Kind))
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
