//! Linear-memory allocation plus one-slot transform result/error buffers.

use std::alloc::{self, Layout};
use std::cell::RefCell;

thread_local! {
    static RESULT: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
    static ERROR: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
}

#[no_mangle]
pub extern "C" fn qjst_alloc(size: usize) -> *mut u8 {
    if size == 0 {
        return std::ptr::null_mut();
    }
    let layout = match Layout::from_size_align(size, 1) {
        Ok(layout) => layout,
        Err(_) => return std::ptr::null_mut(),
    };
    unsafe { alloc::alloc(layout) }
}

#[no_mangle]
pub extern "C" fn qjst_free(ptr: *mut u8, size: usize) {
    if ptr.is_null() || size == 0 {
        return;
    }
    if let Ok(layout) = Layout::from_size_align(size, 1) {
        unsafe { alloc::dealloc(ptr, layout) };
    }
}

#[no_mangle]
pub extern "C" fn qjst_result_ptr() -> *const u8 {
    RESULT.with(|result| result.borrow().as_ptr())
}

#[no_mangle]
pub extern "C" fn qjst_result_len() -> usize {
    RESULT.with(|result| result.borrow().len())
}

#[no_mangle]
pub extern "C" fn qjst_error_ptr() -> *const u8 {
    ERROR.with(|error| error.borrow().as_ptr())
}

#[no_mangle]
pub extern "C" fn qjst_error_len() -> usize {
    ERROR.with(|error| error.borrow().len())
}

#[no_mangle]
pub extern "C" fn qjst_result_free() {
    RESULT.with(|result| {
        let mut buffer = result.borrow_mut();
        buffer.clear();
        buffer.shrink_to_fit();
    });
    ERROR.with(|error| {
        let mut buffer = error.borrow_mut();
        buffer.clear();
        buffer.shrink_to_fit();
    });
}

pub(crate) fn read_utf8<'a>(ptr: *const u8, len: usize) -> Option<&'a str> {
    if len == 0 {
        return Some("");
    }
    if ptr.is_null() {
        return None;
    }
    let bytes = unsafe { std::slice::from_raw_parts(ptr, len) };
    std::str::from_utf8(bytes).ok()
}

pub(crate) fn set_result(bytes: Vec<u8>) {
    RESULT.with(|result| *result.borrow_mut() = bytes);
}

pub(crate) fn set_error(bytes: Vec<u8>) {
    ERROR.with(|error| *error.borrow_mut() = bytes);
}
