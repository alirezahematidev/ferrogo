// FerroGo ABI helper, version 1 (see docs/abi.md).
//
// FerroGo injects this file into every library crate it builds as the
// crate-root module `ferrogo`. User code exports functions with:
//
//     ferrogo::export! {
//         fn add(a: i32, b: i32) -> i32 { a + b }
//     }
//
// Every exported function gets one C-ABI entry point with the same
// machine-level signature on every backend:
//
//     uint32_t ferrogo_v1_call_<name>(const uint8_t *in, size_t len, fg_buf *out)
//
// Arguments arrive as a tagged little-endian value list; the result is
// written to *out as an owned buffer that the host frees with ferrogo_free.
// Panics never unwind across the FFI boundary.
#![allow(dead_code, unused_macros, unused_imports, unused_unsafe)]

use std::cell::RefCell;
use std::fmt::Display;

/// ABI version reported by `ferrogo_abi_version`.
pub const ABI_VERSION: u32 = 1;

/// Call status codes.
pub const STATUS_OK: u32 = 0;
pub const STATUS_ERROR: u32 = 1;
pub const STATUS_PANIC: u32 = 2;
pub const STATUS_ABI: u32 = 3;

/// Wire value tags.
pub const TAG_VOID: u8 = 0x00;
pub const TAG_BOOL: u8 = 0x01;
pub const TAG_I32: u8 = 0x02;
pub const TAG_I64: u8 = 0x03;
pub const TAG_U32: u8 = 0x04;
pub const TAG_U64: u8 = 0x05;
pub const TAG_F32: u8 = 0x06;
pub const TAG_F64: u8 = 0x07;
pub const TAG_STRING: u8 = 0x08;
pub const TAG_BYTES: u8 = 0x09;
pub const TAG_JSON: u8 = 0x0A;

/// Buffer descriptor written to the host's out-slot.
#[repr(C)]
pub struct Buf {
    pub ptr: *mut u8,
    pub len: usize,
}

/// JSON text crossing the boundary. FerroGo validates JSON syntax on the Go
/// side; parse it with serde_json (or any parser) on the Rust side.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Json(pub String);

#[unsafe(no_mangle)]
pub extern "C" fn ferrogo_abi_version() -> u32 {
    ABI_VERSION
}

/// Allocates `len` bytes owned by this module's allocator. Returns null on
/// allocation failure.
#[unsafe(no_mangle)]
pub extern "C" fn ferrogo_alloc(len: usize) -> *mut u8 {
    let mut v: Vec<u8> = Vec::new();
    if v.try_reserve_exact(len).is_err() {
        return std::ptr::null_mut();
    }
    v.resize(len, 0);
    Box::into_raw(v.into_boxed_slice()) as *mut u8
}

/// Frees a buffer returned by `ferrogo_alloc` or written to an out-slot.
///
/// # Safety
/// `ptr` and `len` must describe exactly one such buffer, freed at most once.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn ferrogo_free(ptr: *mut u8, len: usize) {
    if ptr.is_null() || len == 0 {
        return; // zero-length boxes own no allocation
    }
    // SAFETY: the caller guarantees ptr/len came from a Box<[u8]> of length len.
    unsafe { drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(ptr, len))) };
}

thread_local! {
    static LAST_PANIC: RefCell<Option<String>> = const { RefCell::new(None) };
}

/// Writes the most recent panic on this thread ("file:line:col\x1fmessage")
/// to `out`. On WASM, where panics abort, the host calls this after the trap.
///
/// # Safety
/// `out` must point to writable memory for one `Buf` (alignment not required).
#[unsafe(no_mangle)]
pub unsafe extern "C" fn ferrogo_v1_last_panic(out: *mut Buf) -> u32 {
    let msg = LAST_PANIC.with(|p| p.borrow_mut().take()).unwrap_or_default();
    unsafe { write_out(out, STATUS_OK, msg.into_bytes()) }
}

fn install_panic_hook() {
    static ONCE: std::sync::Once = std::sync::Once::new();
    ONCE.call_once(|| {
        // Replace the default hook: it would print to the host's stderr.
        std::panic::set_hook(Box::new(|info| {
            let payload = info.payload();
            let msg = if let Some(s) = payload.downcast_ref::<&str>() {
                (*s).to_string()
            } else if let Some(s) = payload.downcast_ref::<String>() {
                s.clone()
            } else {
                "Box<dyn Any>".to_string()
            };
            let loc = info
                .location()
                .map(|l| format!("{}:{}:{}", l.file(), l.line(), l.column()))
                .unwrap_or_default();
            LAST_PANIC.with(|p| *p.borrow_mut() = Some(format!("{loc}\x1f{msg}")));
        }));
    });
}

/// Writes an owned buffer descriptor to `out` and returns `status`.
///
/// # Safety
/// `out` must point to writable memory for one `Buf` (alignment not required).
pub unsafe fn write_out(out: *mut Buf, status: u32, bytes: Vec<u8>) -> u32 {
    let len = bytes.len();
    let ptr = Box::into_raw(bytes.into_boxed_slice()) as *mut u8;
    // SAFETY: guaranteed by the caller; the slot may be unaligned on WASM.
    unsafe { std::ptr::write_unaligned(out, Buf { ptr, len }) };
    status
}

/// Decoder over an argument buffer.
pub struct Reader<'a> {
    buf: &'a [u8],
    pos: usize,
}

impl<'a> Reader<'a> {
    fn new(buf: &'a [u8]) -> Self {
        Reader { buf, pos: 0 }
    }

    fn take(&mut self, n: usize) -> Result<&'a [u8], String> {
        let end = self.pos.checked_add(n).filter(|&e| e <= self.buf.len());
        match end {
            Some(end) => {
                let s = &self.buf[self.pos..end];
                self.pos = end;
                Ok(s)
            }
            None => Err("truncated argument buffer".to_string()),
        }
    }

    fn u8(&mut self) -> Result<u8, String> {
        Ok(self.take(1)?[0])
    }

    fn u32(&mut self) -> Result<u32, String> {
        let b = self.take(4)?;
        Ok(u32::from_le_bytes([b[0], b[1], b[2], b[3]]))
    }

    fn u64(&mut self) -> Result<u64, String> {
        let b = self.take(8)?;
        let mut a = [0u8; 8];
        a.copy_from_slice(b);
        Ok(u64::from_le_bytes(a))
    }

    fn tag(&mut self, want: u8, ty: &str) -> Result<(), String> {
        let got = self.u8()?;
        if got != want {
            return Err(format!("expected {ty} (tag {want:#04x}), got tag {got:#04x}"));
        }
        Ok(())
    }

    fn bytes(&mut self) -> Result<&'a [u8], String> {
        let n = self.u32()? as usize;
        self.take(n)
    }

    /// Reads the args header and returns the argument count.
    pub fn header(&mut self) -> Result<u32, String> {
        let v = self.u8()?;
        if v != 1 {
            return Err(format!("unsupported wire version {v}"));
        }
        self.u32()
    }

    /// Fails if any input is left over.
    pub fn finish(&self) -> Result<(), String> {
        if self.pos != self.buf.len() {
            return Err(format!("{} unexpected trailing bytes", self.buf.len() - self.pos));
        }
        Ok(())
    }
}

/// Types that can be decoded from a wire value.
pub trait FromWire: Sized {
    fn from_wire(r: &mut Reader<'_>) -> Result<Self, String>;
}

/// Types that can be encoded as a wire value.
pub trait IntoWire {
    fn into_wire(self, out: &mut Vec<u8>);
}

macro_rules! scalar {
    ($t:ty, $tag:expr, $name:expr, $read:ident, $from:expr, $to:expr) => {
        impl FromWire for $t {
            fn from_wire(r: &mut Reader<'_>) -> Result<Self, String> {
                r.tag($tag, $name)?;
                let raw = r.$read()?;
                Ok($from(raw))
            }
        }
        impl IntoWire for $t {
            fn into_wire(self, out: &mut Vec<u8>) {
                out.push($tag);
                out.extend_from_slice(&$to(self).to_le_bytes());
            }
        }
    };
}

scalar!(i32, TAG_I32, "i32", u32, |v: u32| v as i32, |v: i32| v as u32);
scalar!(i64, TAG_I64, "i64", u64, |v: u64| v as i64, |v: i64| v as u64);
scalar!(u32, TAG_U32, "u32", u32, |v: u32| v, |v: u32| v);
scalar!(u64, TAG_U64, "u64", u64, |v: u64| v, |v: u64| v);
scalar!(f32, TAG_F32, "f32", u32, f32::from_bits, f32::to_bits);
scalar!(f64, TAG_F64, "f64", u64, f64::from_bits, f64::to_bits);

impl FromWire for bool {
    fn from_wire(r: &mut Reader<'_>) -> Result<Self, String> {
        r.tag(TAG_BOOL, "bool")?;
        match r.u8()? {
            0 => Ok(false),
            1 => Ok(true),
            b => Err(format!("invalid bool byte {b}")),
        }
    }
}

impl IntoWire for bool {
    fn into_wire(self, out: &mut Vec<u8>) {
        out.push(TAG_BOOL);
        out.push(self as u8);
    }
}

impl FromWire for String {
    fn from_wire(r: &mut Reader<'_>) -> Result<Self, String> {
        r.tag(TAG_STRING, "String")?;
        let b = r.bytes()?;
        std::str::from_utf8(b).map(str::to_owned).map_err(|e| format!("invalid UTF-8 in string: {e}"))
    }
}

impl FromWire for Vec<u8> {
    fn from_wire(r: &mut Reader<'_>) -> Result<Self, String> {
        r.tag(TAG_BYTES, "Vec<u8>")?;
        Ok(r.bytes()?.to_vec())
    }
}

impl FromWire for Json {
    fn from_wire(r: &mut Reader<'_>) -> Result<Self, String> {
        r.tag(TAG_JSON, "Json")?;
        let b = r.bytes()?;
        std::str::from_utf8(b).map(|s| Json(s.to_owned())).map_err(|e| format!("invalid UTF-8 in JSON: {e}"))
    }
}

fn put_len_prefixed(out: &mut Vec<u8>, tag: u8, b: &[u8]) {
    out.push(tag);
    out.extend_from_slice(&(b.len() as u32).to_le_bytes());
    out.extend_from_slice(b);
}

impl IntoWire for String {
    fn into_wire(self, out: &mut Vec<u8>) {
        put_len_prefixed(out, TAG_STRING, self.as_bytes());
    }
}

impl IntoWire for &str {
    fn into_wire(self, out: &mut Vec<u8>) {
        put_len_prefixed(out, TAG_STRING, self.as_bytes());
    }
}

impl IntoWire for Vec<u8> {
    fn into_wire(self, out: &mut Vec<u8>) {
        put_len_prefixed(out, TAG_BYTES, &self);
    }
}

impl IntoWire for Json {
    fn into_wire(self, out: &mut Vec<u8>) {
        put_len_prefixed(out, TAG_JSON, self.0.as_bytes());
    }
}

impl IntoWire for () {
    fn into_wire(self, out: &mut Vec<u8>) {
        out.push(TAG_VOID);
    }
}

/// Converts a function's return value into an encoded result or an error
/// message. Plain values succeed; `Result::Err(e)` reports `e.to_string()`.
pub trait IntoOutcome {
    fn into_outcome(self) -> Result<Vec<u8>, String>;
}

impl<T: IntoWire> IntoOutcome for T {
    fn into_outcome(self) -> Result<Vec<u8>, String> {
        let mut out = Vec::new();
        self.into_wire(&mut out);
        Ok(out)
    }
}

impl<T: IntoWire, E: Display> IntoOutcome for Result<T, E> {
    fn into_outcome(self) -> Result<Vec<u8>, String> {
        match self {
            Ok(v) => v.into_outcome(),
            Err(e) => Err(e.to_string()),
        }
    }
}

/// Decodes arguments, runs the call with panics caught, and writes the result.
///
/// # Safety
/// `input` must point to `len` readable bytes (or `len` must be 0) and `out`
/// must point to writable memory for one `Buf`.
pub unsafe fn dispatch<D, C, R>(input: *const u8, len: usize, out: *mut Buf, nargs: u32, decode: D) -> u32
where
    D: FnOnce(&mut Reader<'_>) -> Result<C, String>,
    C: FnOnce() -> R,
    R: IntoOutcome,
{
    install_panic_hook();
    let bytes: &[u8] = if len == 0 {
        &[]
    } else {
        // SAFETY: guaranteed by the caller.
        unsafe { std::slice::from_raw_parts(input, len) }
    };
    let mut r = Reader::new(bytes);
    let decoded = r.header().and_then(|n| {
        if n != nargs {
            return Err(format!("expected {nargs} argument(s), got {n}"));
        }
        let call = decode(&mut r)?;
        r.finish()?;
        Ok(call)
    });
    let call = match decoded {
        Ok(call) => call,
        Err(msg) => return unsafe { write_out(out, STATUS_ABI, msg.into_bytes()) },
    };
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| call().into_outcome())) {
        Ok(Ok(bytes)) => unsafe { write_out(out, STATUS_OK, bytes) },
        Ok(Err(msg)) => unsafe { write_out(out, STATUS_ERROR, msg.into_bytes()) },
        Err(_) => {
            let msg = LAST_PANIC.with(|p| p.borrow_mut().take()).unwrap_or_default();
            unsafe { write_out(out, STATUS_PANIC, msg.into_bytes()) }
        }
    }
}

/// Writes a function's signature text to `out`.
///
/// # Safety
/// `out` must point to writable memory for one `Buf`.
pub unsafe fn signature(out: *mut Buf, name: &str, params: &[&str], ret: &str) -> u32 {
    let mut s = format!("fn {name}({})", params.join(", "));
    if !ret.is_empty() {
        s.push_str(" -> ");
        s.push_str(ret);
    }
    unsafe { write_out(out, STATUS_OK, s.into_bytes()) }
}

/// Exports functions to Go. See the module documentation.
macro_rules! export {
    ($(
        $(#[$meta:meta])*
        $vis:vis fn $name:ident ( $( $arg:ident : $ty:ty ),* $(,)? ) $( -> $ret:ty )? $body:block
    )*) => {$(
        $(#[$meta])*
        $vis fn $name( $( $arg : $ty ),* ) $( -> $ret )? $body

        const _: () = {
            #[unsafe(export_name = concat!("ferrogo_v1_call_", stringify!($name)))]
            unsafe extern "C" fn call(input: *const u8, len: usize, out: *mut $crate::ferrogo::Buf) -> u32 {
                const NARGS: u32 = { let names: &[&str] = &[$( stringify!($arg) ),*]; names.len() as u32 };
                unsafe {
                    $crate::ferrogo::dispatch(input, len, out, NARGS, |_r| {
                        $( let $arg: $ty = <$ty as $crate::ferrogo::FromWire>::from_wire(_r)?; )*
                        Ok(move || $name( $( $arg ),* ))
                    })
                }
            }

            #[unsafe(export_name = concat!("ferrogo_v1_sig_", stringify!($name)))]
            unsafe extern "C" fn sig(out: *mut $crate::ferrogo::Buf) -> u32 {
                #[allow(path_statements, unused_must_use)]
                let ret: &str = { "" $( ; stringify!($ret) )? };
                unsafe {
                    $crate::ferrogo::signature(out, stringify!($name),
                        &[$( concat!(stringify!($arg), ": ", stringify!($ty)) ),*], ret)
                }
            }
        };
    )*};
}
pub(crate) use export;

impl FromWire for () {
    fn from_wire(r: &mut Reader<'_>) -> Result<Self, String> {
        r.tag(TAG_VOID, "()")
    }
}

#[cfg(target_arch = "wasm32")]
#[link(wasm_import_module = "ferrogo")]
unsafe extern "C" {
    fn ferrogo_host_call(name: *const u8, name_len: usize, input: *const u8, input_len: usize, out: *mut Buf) -> u32;
}

/// Calls a Go host function registered with ferrogo.WithHostFunction.
/// `input` is an encoded argument list; the result is an encoded value.
/// Host functions are only available on the WASM backend.
pub fn host_call(name: &str, input: Vec<u8>) -> Result<Vec<u8>, String> {
    #[cfg(target_arch = "wasm32")]
    {
        let mut out = Buf { ptr: std::ptr::null_mut(), len: 0 };
        // SAFETY: all pointers are valid for the duration of the call; the
        // host writes a buffer it allocated with ferrogo_alloc into `out`.
        let status = unsafe { ferrogo_host_call(name.as_ptr(), name.len(), input.as_ptr(), input.len(), &mut out) };
        let bytes = if out.ptr.is_null() || out.len == 0 {
            Vec::new()
        } else {
            // SAFETY: the host allocated exactly out.len bytes with
            // ferrogo_alloc, so this module's allocator owns them.
            unsafe { Box::from_raw(std::ptr::slice_from_raw_parts_mut(out.ptr, out.len)) }.into_vec()
        };
        if status == STATUS_OK {
            Ok(bytes)
        } else {
            Err(String::from_utf8_lossy(&bytes).into_owned())
        }
    }
    #[cfg(not(target_arch = "wasm32"))]
    {
        let _ = input;
        Err(format!("host function {name}: host functions are only available on the WASM backend"))
    }
}

/// Decodes a host function's result.
pub fn host_result<T: FromWire>(bytes: &[u8]) -> Result<T, String> {
    let mut r = Reader::new(bytes);
    let v = T::from_wire(&mut r)?;
    r.finish()?;
    Ok(v)
}

/// Declares Go host functions callable from Rust. Each declaration becomes a
/// Rust function returning Result<T, String>; Err carries the Go error.
///
///     ferrogo::import! {
///         fn lookup_user(id: u64) -> String;
///     }
macro_rules! import {
    ($(
        $(#[$meta:meta])*
        $vis:vis fn $name:ident ( $( $arg:ident : $ty:ty ),* $(,)? ) $( -> $ret:ty )? ;
    )*) => {$(
        $(#[$meta])*
        $vis fn $name( $( $arg : $ty ),* ) -> Result<$crate::ferrogo::__ret!($($ret)?), String> {
            let mut input: Vec<u8> = vec![1];
            let names: &[&str] = &[$( stringify!($arg) ),*];
            input.extend_from_slice(&(names.len() as u32).to_le_bytes());
            $( $crate::ferrogo::IntoWire::into_wire($arg, &mut input); )*
            let bytes = $crate::ferrogo::host_call(stringify!($name), input)?;
            $crate::ferrogo::host_result(&bytes)
        }
    )*};
}
pub(crate) use import;

macro_rules! __ret {
    () => { () };
    ($t:ty) => { $t };
}
pub(crate) use __ret;
