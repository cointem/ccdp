// Trusted bootstrap. Only the returned receiver and the lexical send closure
// can speak the protocol; neither is reachable from the user program.
((names, send) => {
  'use strict';
  delete globalThis.__send;
  const stringify = JSON.stringify, keys = Reflect.ownKeys;
  const descriptor = Object.getOwnPropertyDescriptor, proto = Object.getPrototypeOf;
  const create = Object.create, freeze = Object.freeze;
  const isArray = Array.isArray, finite = Number.isFinite;
  const integer = Number.isInteger, safeInteger = Number.isSafeInteger;
  const parse = JSON.parse, PromiseClass = Promise, ErrorClass = Error;
  const define = Object.defineProperty;
  const setProto = Object.setPrototypeOf, objectProto = Object.prototype;
  const charCodeAt = Function.prototype.call.bind(String.prototype.charCodeAt);
  const maxFrame = __MAX_FRAME__, maxResults = __MAX_RESULTS__, maxCalls = __MAX_CALLS__;
  const pending = create(null);
  let serial = 0, emitted = 2, emissionCount = 0;
  function bytes(text) {
    let size = 0;
    for (let i = 0; i < text.length; i++) {
      const c = charCodeAt(text, i);
      if (c < 0x80) size++;
      else if (c < 0x800) size += 2;
      else if (c >= 0xd800 && c <= 0xdbff && i + 1 < text.length &&
               charCodeAt(text, i + 1) >= 0xdc00 && charCodeAt(text, i + 1) <= 0xdfff) { size += 4; i++; }
      else size += 3;
    }
    return size;
  }
  function json(value) {
    const seen = []; let nodes = 0;
    function copy(v, depth) {
      if (++nodes > 100000 || depth > 64) throw new ErrorClass('JSON value is too complex');
      if (v === null || typeof v === 'string' || typeof v === 'boolean') return v;
      if (typeof v === 'number' && finite(v)) {
        if (integer(v) && !safeInteger(v)) throw new ErrorClass('Integer exceeds JavaScript exact range; use a direct tool call for exact JSON integers');
        return v;
      }
      if (typeof v !== 'object') throw new ErrorClass('Expected finite JSON values; undefined, functions, symbols and BigInt are not supported');
      for (let i=0;i<seen.length;i++) if (seen[i]===v) throw new ErrorClass('Cyclic JSON value');
      const array = isArray(v);
      if (!array && proto(v) !== objectProto && proto(v) !== null) throw new ErrorClass('Expected plain JSON object');
      seen[seen.length]=v;
      const out = array ? setProto([],null) : create(null);
      const own=keys(v);
      for (let i=0;i<own.length;i++) {
        const key=own[i];
        if (array && key === 'length') continue;
        const d = descriptor(v, key);
        if (typeof key !== 'string' || !('value' in d)) throw new ErrorClass('JSON getters and symbol keys are not supported');
        if (!d.enumerable) continue;
        if (array && (!/^(0|[1-9][0-9]*)$/.test(key) || +key >= v.length)) throw new ErrorClass('Array properties are not JSON values');
        out[key] = copy(d.value, depth + 1);
      }
      if (array && out.length !== v.length) throw new ErrorClass('Sparse arrays are not JSON values');
      if (array) for (let i = 0; i < v.length; i++) if (!descriptor(v, String(i))) throw new ErrorClass('Sparse arrays are not JSON values');
      seen.length--; return out;
    }
    return stringify(copy(value, 0));
  }
  const tools = create(null);
  for (const name of names) {
    define(tools, name, {enumerable: true, value: args => {
      if (!args || typeof args !== 'object' || isArray(args)) throw new ErrorClass('Tool arguments must be a JSON object');
      const encoded = json(args);
      if (serial >= maxCalls) {send('{"type":"limit","error":"Tool call limit exceeded"}');throw new ErrorClass('Tool call limit exceeded');}
      const id = serial + 1;
      const frame = '{"type":"call","id":' + id + ',"name":' + stringify(name) + ',"args":' + encoded + '}';
      if (bytes(frame) > maxFrame) throw new ErrorClass('Tool arguments exceed the 1 MiB frame limit');
      serial = id;
      return new PromiseClass((resolve, reject) => {
        pending[id] = {resolve, reject};
        send(frame);
      });
    }});
  }
  define(globalThis, 'tools', {value: freeze(tools)});
  define(globalThis, 'text', {value: value => {
    const encoded = json(value);
    const frame = '{"type":"emit","value":' + encoded + '}';
    if (bytes(frame) > maxFrame) throw new ErrorClass('text output exceeds the 1 MiB frame limit');
    emitted += bytes(encoded) + (emissionCount++ > 0 ? 1 : 0);
    if (emitted > maxResults) throw new ErrorClass('text output limit exceeded');
    send(frame);
  }});
  return frame => {
    const p = pending[frame.id];
    if (!p) throw new ErrorClass('Unknown tool response');
    delete pending[frame.id];
    if (frame.result.status === 'success' && !frame.result.data_error) p.resolve(frame.result);
    else {
      const error = new ErrorClass(frame.result.data_error || frame.result.text || frame.result.status);
      error.name = 'ToolError'; error.kind = frame.result.data_error ? 'invalid_data' : 'execution'; error.call_id = frame.result.call_id;
      error.status = frame.result.status; error.result = frame.result;
      p.reject(error);
    }
  };
})(__NAMES__, __send)
