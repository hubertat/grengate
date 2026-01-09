local req = GATE_HTTP->homebridge->RequestBody

local resp = {}

function ReadLight(clu, id)
	local Light = {}

	-- temporary workaround for fibaro wall plug
	if id == "TMP0001" then
		Light.State = CLU_GRENTON_Rs->fib_wall1
		return Light
	end

	if _G[clu]:execute(0, id .. ":get(0)") == 1 then
		Light.State = true
	else
		Light.State = false
	end

	return Light
end

function ReadThermo(clu, thermo, sensor)
	local Thermo = {}

	Thermo.TempMin = _G[clu]:execute(0, thermo .. ":get(8)")
	Thermo.TempMax = _G[clu]:execute(0, thermo .. ":get(9)")
	Thermo.TempTarget = _G[clu]:execute(0, thermo .. ":get(12)")
	Thermo.TempHoliday = _G[clu]:execute(0, thermo .. ":get(4)")
	Thermo.TempSetpoint = _G[clu]:execute(0, thermo .. ":get(3)")
	Thermo.Mode = _G[clu]:execute(0, thermo .. ":get(8)")
	Thermo.State = _G[clu]:execute(0, thermo .. ":get(6)")

	Thermo.TempCurrent = _G[clu]:execute(0, "getVar(\"" .. sensor .. "\")")

	return Thermo
end

function ReadShutter(clu, id)
	local Shutter = {}

	Shutter.MaxTime = _G[clu]:execute(0, id .. ":get(3)")
	Shutter.State = _G[clu]:execute(0, id .. ":get(2)")

	return Shutter
end

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

	_G[clu]:execute(0, id .. ":set(3, " .. thermo.TempSetpoint .. ")")
	_G[clu]:execute(0, id .. ":set(6, " .. thermo.State .. ")")
	_G[clu]:execute(0, id .. ":set(8, " .. thermo.Mode .. ")")

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


-- Check if req is an array or single object
if req ~= nil then
	-- Detect if it's an array by checking if first element has numeric key
	local isArray = (type(req[1]) == "table")

	if isArray then
		-- Handle array of requests (batch mode)
		resp = {}
		for i, rl in ipairs(req) do
			if (rl.Clu ~= nil) and (_G[rl.Clu] ~= nil) then
				-- Wrap processing in pcall to catch errors
				local success, result = pcall(function()
				local singleResp = {}
				singleResp.Clu = rl.Clu
				singleResp.Id = rl.Id
				singleResp.Kind = rl.Kind

				if rl.Kind == "Light" then
					SetLight(rl.Clu, rl.Id, rl.Light)
					singleResp.Light = ReadLight(rl.Clu, rl.Id)
				end

				if rl.Kind == "Thermo" then
					SetThermo(rl.Clu, rl.Id, rl.Thermo)
					singleResp.Thermo = ReadThermo(rl.Clu, rl.Id, rl.Sensor)
				end

				if rl.Kind == "Shutter" then
					SetShutter(rl.Clu, rl.Id, rl.Cmd)
					singleResp.Shutter = ReadShutter(rl.Clu, rl.Id)
				end

				return singleResp
				end)

				if success then
					resp[i] = result
				else
					-- Error occurred, log it and return error response
					resp[i] = {
						Clu = rl.Clu,
						Id = rl.Id,
						Kind = rl.Kind,
						Error = "Lua error: " .. tostring(result)
					}
				end
			end
		end
	else
		-- Handle single request (backward compatible)
		if (req.Clu ~= nil) and (_G[req.Clu] ~= nil) then
			-- Wrap processing in pcall to catch errors
			local success, result = pcall(function()
				local singleResp = {}
				singleResp.Clu = req.Clu
				singleResp.Id = req.Id
				singleResp.Kind = req.Kind

				if req.Kind == "Light" then
					SetLight(req.Clu, req.Id, req.Light)
					singleResp.Light = ReadLight(req.Clu, req.Id)
				end

				if req.Kind == "Thermo" then
					SetThermo(req.Clu, req.Id, req.Thermo)
					singleResp.Thermo = ReadThermo(req.Clu, req.Id, req.Sensor)
				end

				if req.Kind == "Shutter" then
					SetShutter(req.Clu, req.Id, req.Cmd)
					singleResp.Shutter = ReadShutter(req.Clu, req.Id)
				end

				return singleResp
			end)

			if success then
				resp = result
			else
				-- Error occurred, return error response
				resp = {
					Clu = req.Clu,
					Id = req.Id,
					Kind = req.Kind,
					Error = "Lua error: " .. tostring(result)
				}
			end
		end
	end
end

GATE_HTTP->homebridge->SetResponseBody(resp)
GATE_HTTP->homebridge->SendResponse()
