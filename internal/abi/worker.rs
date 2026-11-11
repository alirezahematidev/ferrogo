// FerroGo worker: serves calls to a FerroGo ABI library (cdylib) from a
// separate process, for the native-process backend (docs/abi.md §11).
//
// Usage: worker <library path>
//
// Protocol on stdin/stdout (little-endian):
//   request:  op:u8 (1 = call, 2 = signature) name_len:u32 name in_len:u32 input
//   response: status:u32 out_len:u32 output
// Status 255 means the function does not exist. EOF on stdin ends the worker.
//
// Before serving, the worker keeps a private copy of stdout for the protocol
// and points stdout at stderr, so println! in library code cannot corrupt
// the protocol stream.

use std::ffi::{c_char, c_void, CString};
use std::io::{Read, Write};

#[repr(C)]
struct Buf {
    ptr: *mut u8,
    len: usize,
}

type CallFn = unsafe extern "C" fn(*const u8, usize, *mut Buf) -> u32;
type SigFn = unsafe extern "C" fn(*mut Buf) -> u32;
type FreeFn = unsafe extern "C" fn(*mut u8, usize);
type VersionFn = unsafe extern "C" fn() -> u32;

const STATUS_NOT_FOUND: u32 = 255;

#[cfg(unix)]
mod sys {
    use super::*;
    use std::os::unix::io::FromRawFd;

    #[cfg_attr(target_os = "linux", link(name = "dl"))]
    unsafe extern "C" {
        fn dlopen(path: *const c_char, flags: i32) -> *mut c_void;
        fn dlsym(handle: *mut c_void, name: *const c_char) -> *mut c_void;
        fn dup(fd: i32) -> i32;
        fn dup2(old: i32, new: i32) -> i32;
    }
    const RTLD_NOW: i32 = 2;

    pub fn open(path: &str) -> Result<*mut c_void, String> {
        let c = CString::new(path).map_err(|e| e.to_string())?;
        // SAFETY: c is a valid NUL-terminated path.
        let h = unsafe { dlopen(c.as_ptr(), RTLD_NOW) };
        if h.is_null() { Err(format!("cannot load {path}")) } else { Ok(h) }
    }

    pub fn symbol(h: *mut c_void, name: &str) -> *mut c_void {
        match CString::new(name) {
            // SAFETY: h is a live handle and n a valid C string.
            Ok(n) => unsafe { dlsym(h, n.as_ptr()) },
            Err(_) => std::ptr::null_mut(),
        }
    }

    /// Returns a private handle to the original stdout and points fd 1 at stderr.
    pub fn protocol_output() -> std::fs::File {
        // SAFETY: plain descriptor duplication on valid descriptors.
        unsafe {
            let fd = dup(1);
            dup2(2, 1);
            std::fs::File::from_raw_fd(fd)
        }
    }
}

#[cfg(windows)]
mod sys {
    use super::*;
    use std::os::windows::io::FromRawHandle;

    #[link(name = "kernel32")]
    unsafe extern "system" {
        fn LoadLibraryW(name: *const u16) -> *mut c_void;
        fn GetProcAddress(module: *mut c_void, name: *const c_char) -> *mut c_void;
        fn GetStdHandle(which: u32) -> *mut c_void;
        fn SetStdHandle(which: u32, handle: *mut c_void) -> i32;
    }
    const STD_OUTPUT_HANDLE: u32 = -11i32 as u32;
    const STD_ERROR_HANDLE: u32 = -12i32 as u32;

    pub fn open(path: &str) -> Result<*mut c_void, String> {
        let wide: Vec<u16> = path.encode_utf16().chain(Some(0)).collect();
        // SAFETY: wide is a valid NUL-terminated UTF-16 string.
        let h = unsafe { LoadLibraryW(wide.as_ptr()) };
        if h.is_null() { Err(format!("cannot load {path}")) } else { Ok(h) }
    }

    pub fn symbol(h: *mut c_void, name: &str) -> *mut c_void {
        match CString::new(name) {
            // SAFETY: h is a live module and n a valid C string.
            Ok(n) => unsafe { GetProcAddress(h, n.as_ptr()) },
            Err(_) => std::ptr::null_mut(),
        }
    }

    /// Returns the original stdout for the protocol and points the process's
    /// standard output at stderr (Rust looks the handle up on every write).
    pub fn protocol_output() -> std::fs::File {
        // SAFETY: valid standard handles of this process.
        unsafe {
            let out = GetStdHandle(STD_OUTPUT_HANDLE);
            SetStdHandle(STD_OUTPUT_HANDLE, GetStdHandle(STD_ERROR_HANDLE));
            std::fs::File::from_raw_handle(out)
        }
    }
}

fn read_u32(r: &mut impl Read) -> std::io::Result<u32> {
    let mut b = [0u8; 4];
    r.read_exact(&mut b)?;
    Ok(u32::from_le_bytes(b))
}

fn read_bytes(r: &mut impl Read, max: u32) -> std::io::Result<Vec<u8>> {
    let n = read_u32(r)?;
    if n > max {
        return Err(std::io::Error::new(std::io::ErrorKind::InvalidData, "frame too large"));
    }
    let mut v = vec![0u8; n as usize];
    r.read_exact(&mut v)?;
    Ok(v)
}

fn main() {
    let path = match std::env::args().nth(1) {
        Some(p) => p,
        None => {
            eprintln!("ferrogo worker: missing library path");
            std::process::exit(2);
        }
    };
    let lib = match sys::open(&path) {
        Ok(h) => h,
        Err(e) => {
            eprintln!("ferrogo worker: {e}");
            std::process::exit(2);
        }
    };
    let sym = |name: &str| sys::symbol(lib, name);
    let version = sym("ferrogo_abi_version");
    let free = sym("ferrogo_free");
    if version.is_null() || free.is_null() {
        eprintln!("ferrogo worker: {path} is not a FerroGo ABI library");
        std::process::exit(3);
    }
    // SAFETY: the symbols come from a FerroGo ABI library (checked above),
    // whose exports have exactly these signatures (docs/abi.md §3).
    let (version, free): (VersionFn, FreeFn) = unsafe { (std::mem::transmute(version), std::mem::transmute(free)) };
    if unsafe { version() } != 1 {
        eprintln!("ferrogo worker: unsupported ABI version");
        std::process::exit(3);
    }

    let mut out = std::io::BufWriter::new(sys::protocol_output());
    let stdin = std::io::stdin();
    let mut input = stdin.lock();
    const MAX: u32 = 64 << 20;
    loop {
        let mut op = [0u8; 1];
        if input.read_exact(&mut op).is_err() {
            return; // EOF: the host closed the module
        }
        let (name, args) = match (read_bytes(&mut input, 4096), read_bytes(&mut input, MAX)) {
            (Ok(n), Ok(a)) => (String::from_utf8_lossy(&n).into_owned(), a),
            _ => std::process::exit(4),
        };
        let mut buf = Buf { ptr: std::ptr::null_mut(), len: 0 };
        let status = match op[0] {
            1 => {
                let f = sym(&format!("ferrogo_v1_call_{name}"));
                if f.is_null() {
                    STATUS_NOT_FOUND
                } else {
                    // SAFETY: ferrogo_v1_call_* has the CallFn signature.
                    let f: CallFn = unsafe { std::mem::transmute(f) };
                    unsafe { f(args.as_ptr(), args.len(), &mut buf) }
                }
            }
            2 => {
                let f = sym(&format!("ferrogo_v1_sig_{name}"));
                if f.is_null() {
                    STATUS_NOT_FOUND
                } else {
                    // SAFETY: ferrogo_v1_sig_* has the SigFn signature.
                    let f: SigFn = unsafe { std::mem::transmute(f) };
                    unsafe { f(&mut buf) }
                }
            }
            _ => std::process::exit(4),
        };
        let data: &[u8] = if buf.ptr.is_null() || buf.len == 0 {
            &[]
        } else {
            // SAFETY: the library wrote an owned buffer of buf.len bytes.
            unsafe { std::slice::from_raw_parts(buf.ptr, buf.len) }
        };
        let ok = out.write_all(&status.to_le_bytes()).is_ok()
            && out.write_all(&(data.len() as u32).to_le_bytes()).is_ok()
            && out.write_all(data).is_ok()
            && out.flush().is_ok();
        if !buf.ptr.is_null() {
            // SAFETY: the buffer came from the library and is freed once.
            unsafe { free(buf.ptr, buf.len) };
        }
        if !ok {
            return; // the host went away
        }
    }
}
