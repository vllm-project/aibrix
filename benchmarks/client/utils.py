import json
from typing import List, Any, Dict, Set, Tuple
import threading

class Mailbox:
    def __init__(self):
        self.mailbox = list()
        self.running = False

    def add(self, message):
        self.mailbox.append(message)
        
def load_workload(input_path: str) -> List[Any]:
    load_struct = None
    if input_path.endswith(".jsonl"):
        with open(input_path, "r") as file:
            load_struct = [json.loads(line) for line in file]
    else:
        with open(input_path, "r") as file:
            load_struct = json.load(file)
    return load_struct

    
def try_add_running_task(session_id: str, 
                       mailbox_map: Dict[str, Mailbox],
                       session_lock: threading.Lock,
                       *task_args,
                    ) -> bool:
    if session_id is not None:
        with session_lock:
            if session_id not in mailbox_map:
                mailbox_map[session_id] = Mailbox()
            if mailbox_map[session_id].running == True:
                mailbox_map[session_id].mailbox.append(task_args)
                return False
            else:
                mailbox_map[session_id].running == True
                return True
    return True
            

def try_remove_next_task(session_id: str, 
                        mailbox_map: Dict[str, Mailbox],
                        session_lock: threading.Lock = None):
    if session_id is not None:
        with session_lock:
            if session_id in mailbox_map:
                if len(mailbox_map[session_id].mailbox) > 0:
                    task = mailbox_map[session_id].mailbox.pop(0)
                    return task
                else:
                    mailbox_map[session_id].running = False
    return None

# Function to wrap the prompt into OpenAI's chat completion message format.
def prepare_prompt(prompt: str, 
                   session_id: str = None, 
                   history: Dict = None,
                   history_lock: threading.Lock = None) -> List[Dict]:
    """
    Wrap the prompt into OpenAI's chat completion message format.

    :param prompt: The user prompt to be converted.
    :param session_id: Optional session ID for conversation history
    :param history: Dictionary containing conversation history
    :param history_lock: Lock for protecting history access
    :return: A list containing chat completion messages.
    """
    if session_id is not None and history is not None:
        with history_lock:
            past_history = history.get(session_id, [])
            user_message = {"role": "user", "content": f"{prompt}"}
            past_history.append(user_message) 
            history[session_id] = past_history
            return list(past_history)
    else:    
        user_message = {"role": "user", "content": prompt}
        return [user_message]
    
def update_response(response: str, 
                    session_id: str = None, 
                    history: Dict = None,
                    history_lock: threading.Lock = None):
    """
    Update the conversation history with the assistant's response.

    :param response: The assistant's response to be added to history
    :param session_id: Optional session ID for conversation history
    :param history: Dictionary containing conversation history
    :param history_lock: Lock for protecting history access
    """
    if session_id is not None and history is not None:
        with history_lock:
            past_history = history.get(session_id, [])
            assistant_message = {"role": "assistant", "content": f"{response}"}
            past_history.append(assistant_message)
            history[session_id] = past_history 