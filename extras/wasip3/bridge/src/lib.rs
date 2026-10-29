//! Bridge component for container2wasm WASI 0.3 images.
//!
//! The component sits between the host and the emulator core module:
//!
//! * it exports `wasi:cli/run` and drives the emulator (`c2w:bridge/vm.run`);
//! * it pulls call requests made by programs running inside the guest
//!   (`c2w:bridge/vm.next-request`), performs the corresponding import call
//!   on the host and sends the result back (`c2w:bridge/vm.complete`);
//! * it exports the interfaces of the container's WIT world and forwards
//!   calls into the guest (`c2w:bridge/vm.invoke`).
//!
//! Everything that depends on the container's WIT world lives in
//! `generated.rs`, produced by `cmd/wit-bridge-gen`. This file contains the
//! world independent runtime: the request pump and the JSON helpers used by
//! the generated code. The JSON mapping of WIT values is documented in
//! docs/wasip3-bridge-protocol.md.

#![allow(dead_code, unused_imports, clippy::all)]

mod generated;

pub mod runtime {
    use crate::generated::c2w::bridge::vm;
    use serde_json::{json, Map, Value};

    /// Implementation of `wasi:cli/run`.
    pub async fn run() -> Result<(), ()> {
        wit_bindgen::spawn_local(pump());
        vm::run().await
    }

    /// Fetch requests issued by the guest and handle them concurrently.
    async fn pump() {
        loop {
            let request = vm::next_request().await;
            wit_bindgen::spawn_local(handle_request(request));
        }
    }

    async fn handle_request(raw: String) {
        let (id, response) = match parse_request(&raw) {
            Ok((id, iface, func, args)) => {
                let result = crate::generated::dispatch(&iface, &func, &args).await;
                (id, result)
            }
            Err((id, e)) => (id, Err(e)),
        };
        let doc = match response {
            Ok(v) => json!({ "result": v }),
            Err(e) => json!({ "error": e }),
        };
        vm::complete(id, &doc.to_string());
    }

    type ParsedRequest = (u32, String, String, Vec<Value>);

    fn parse_request(raw: &str) -> Result<ParsedRequest, (u32, String)> {
        let v: Value = serde_json::from_str(raw).map_err(|e| (0, format!("invalid request: {e}")))?;
        let id = v
            .get("id")
            .and_then(Value::as_u64)
            .ok_or_else(|| (0, "request without id".to_string()))? as u32;
        let req = v.get("request").ok_or_else(|| (id, "request without body".to_string()))?;
        let iface = req
            .get("interface")
            .and_then(Value::as_str)
            .ok_or_else(|| (id, "request without interface".to_string()))?
            .to_string();
        let func = req
            .get("function")
            .and_then(Value::as_str)
            .ok_or_else(|| (id, "request without function".to_string()))?
            .to_string();
        let args = match req.get("args") {
            None | Some(Value::Null) => Vec::new(),
            Some(Value::Array(a)) => a.clone(),
            Some(_) => return Err((id, "args must be an array".to_string())),
        };
        Ok((id, iface, func, args))
    }

    /// Forward a call of an exported function into the guest.
    pub async fn invoke(iface: &str, func: &str, args: Vec<Value>) -> Result<Value, String> {
        let req = json!({ "interface": iface, "function": func, "args": args }).to_string();
        let raw = vm::invoke(req).await;
        let v: Value = serde_json::from_str(&raw).map_err(|e| format!("invalid guest response: {e}"))?;
        if let Some(e) = v.get("error") {
            return Err(e.as_str().map(str::to_string).unwrap_or_else(|| e.to_string()));
        }
        Ok(v.get("result").cloned().unwrap_or(Value::Null))
    }

    /// Called by generated export shims when the guest returned an error for
    /// a function whose result type cannot carry it.
    pub fn guest_error(iface: &str, func: &str, err: &str) -> ! {
        panic!("c2w bridge: guest failed to handle {iface}.{func}: {err}")
    }

    // ---- JSON helpers used by the generated code --------------------------

    pub fn arg<'a>(args: &'a [Value], i: usize, name: &str) -> Result<&'a Value, String> {
        args.get(i).ok_or_else(|| format!("missing argument {name}"))
    }

    pub fn bool_from_json(v: &Value) -> Result<bool, String> {
        v.as_bool().ok_or_else(|| format!("expected bool, got {v}"))
    }

    pub fn u64_from_json(v: &Value) -> Result<u64, String> {
        if let Some(n) = v.as_u64() {
            return Ok(n);
        }
        if let Some(s) = v.as_str() {
            return s.parse().map_err(|_| format!("expected unsigned integer, got {v}"));
        }
        Err(format!("expected unsigned integer, got {v}"))
    }

    pub fn s64_from_json(v: &Value) -> Result<i64, String> {
        if let Some(n) = v.as_i64() {
            return Ok(n);
        }
        if let Some(s) = v.as_str() {
            return s.parse().map_err(|_| format!("expected integer, got {v}"));
        }
        Err(format!("expected integer, got {v}"))
    }

    pub fn uint_from_json<T: TryFrom<u64>>(v: &Value, bits: u32) -> Result<T, String> {
        let n = u64_from_json(v)?;
        T::try_from(n).map_err(|_| format!("integer {n} does not fit in u{bits}"))
    }

    pub fn sint_from_json<T: TryFrom<i64>>(v: &Value, bits: u32) -> Result<T, String> {
        let n = s64_from_json(v)?;
        T::try_from(n).map_err(|_| format!("integer {n} does not fit in s{bits}"))
    }

    pub fn f64_from_json(v: &Value) -> Result<f64, String> {
        if let Some(f) = v.as_f64() {
            return Ok(f);
        }
        match v.as_str() {
            Some("nan") | Some("NaN") => Ok(f64::NAN),
            Some("inf") | Some("Infinity") => Ok(f64::INFINITY),
            Some("-inf") | Some("-Infinity") => Ok(f64::NEG_INFINITY),
            _ => Err(format!("expected number, got {v}")),
        }
    }

    pub fn f32_from_json(v: &Value) -> Result<f32, String> {
        f64_from_json(v).map(|f| f as f32)
    }

    pub fn float_to_json(f: f64) -> Value {
        if f.is_nan() {
            Value::String("nan".into())
        } else if f.is_infinite() {
            Value::String(if f > 0.0 { "inf" } else { "-inf" }.into())
        } else {
            json!(f)
        }
    }

    pub fn char_from_json(v: &Value) -> Result<char, String> {
        let s = v.as_str().ok_or_else(|| format!("expected one character string, got {v}"))?;
        let mut it = s.chars();
        match (it.next(), it.next()) {
            (Some(c), None) => Ok(c),
            _ => Err(format!("expected one character string, got {v}")),
        }
    }

    pub fn string_from_json(v: &Value) -> Result<String, String> {
        v.as_str().map(str::to_string).ok_or_else(|| format!("expected string, got {v}"))
    }

    pub fn bytes_from_json(v: &Value) -> Result<Vec<u8>, String> {
        use base64::Engine;
        match v {
            Value::String(s) => base64::engine::general_purpose::STANDARD
                .decode(s)
                .map_err(|e| format!("invalid base64 for list<u8>: {e}")),
            Value::Array(a) => a.iter().map(|x| uint_from_json::<u8>(x, 8)).collect(),
            _ => Err(format!("expected base64 string for list<u8>, got {v}")),
        }
    }

    pub fn bytes_to_json(b: &[u8]) -> Value {
        use base64::Engine;
        Value::String(base64::engine::general_purpose::STANDARD.encode(b))
    }

    pub fn array<'a>(v: &'a Value) -> Result<&'a Vec<Value>, String> {
        v.as_array().ok_or_else(|| format!("expected array, got {v}"))
    }

    pub fn object<'a>(v: &'a Value) -> Result<&'a Map<String, Value>, String> {
        v.as_object().ok_or_else(|| format!("expected object, got {v}"))
    }

    pub fn field<'a>(o: &'a Map<String, Value>, name: &str) -> Result<&'a Value, String> {
        o.get(name).ok_or_else(|| format!("missing field {name:?}"))
    }

    pub fn tuple_elem<'a>(a: &'a [Value], i: usize, len: usize) -> Result<&'a Value, String> {
        if a.len() != len {
            return Err(format!("expected tuple of {len} elements, got {}", a.len()));
        }
        Ok(&a[i])
    }
}
